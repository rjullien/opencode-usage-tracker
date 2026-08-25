package handler

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	// Embeds the timezone database in the binary. The runtime image is FROM
	// scratch and carries no /usr/share/zoneinfo, so without this
	// time.LoadLocation("Europe/Paris") fails, displayTZ falls back, and every
	// displayed hour is wrong. The import belongs here, next to the code that
	// depends on it, rather than in main.
	_ "time/tzdata"

	"github.com/rjullien/opencode-usage-tracker/internal/opencode"
)

// templatesFS keeps the templates in the package that renders them, so tests can
// exercise the real markup rather than a copy.
//
//go:embed templates
var templatesFS embed.FS

// displayTZ is resolved once at startup. The binary embeds the timezone database
// (see the time/tzdata import in cmd/dashboard), so this works in a scratch image.
var displayTZ = loadDisplayTZ()

func loadDisplayTZ() *time.Location {
	if loc, err := time.LoadLocation("Europe/Paris"); err == nil {
		return loc
	}
	return time.UTC
}

// Poller interface for getting statuses.
type Poller interface {
	Statuses() []opencode.AgentStatus
}

// Handler serves the dashboard and API.
type Handler struct {
	tmpl   *template.Template
	poller Poller

	// now is injectable so the rendered budget can be asserted deterministically.
	now func() time.Time
}

// New creates a Handler with embedded templates.
func New(poller Poller) *Handler {
	funcMap := template.FuncMap{
		"levelLabel": levelLabel,
		"fmtTime":    fmtTime,
		"fmtDate":    fmtDate,
		"fmtDay":     fmtDay,
		"fmtDur":     fmtDuration,
		"fmtDays":    fmtDays,
		"fmtRate":    fmtRate,
		"deltaPts":   deltaPts,
		"clamp":      clamp,
	}

	tmpl := template.Must(
		template.New("").Funcs(funcMap).ParseFS(templatesFS, "templates/*.html"),
	)

	return &Handler{tmpl: tmpl, poller: poller, now: time.Now}
}

// WindowView is a quota window plus its budget position. JSON field names match
// the previous API shape so existing consumers keep working; budget and level
// are additions.
type WindowView struct {
	Name     string          `json:"name"`
	Kind     string          `json:"kind"`
	Status   string          `json:"status"`
	Percent  int             `json:"percent"`
	ResetsAt time.Time       `json:"resetsAt"`
	ResetIn  time.Duration   `json:"resetIn"`
	Level    opencode.Level  `json:"level"`
	Budget   opencode.Budget `json:"budget"`
}

// AgentView is one subscription.
type AgentView struct {
	Label     string         `json:"label"`
	Windows   []WindowView   `json:"windows,omitempty"`
	Error     string         `json:"error,omitempty"`
	FetchedAt time.Time      `json:"fetchedAt"`
	Level     opencode.Level `json:"level"`

	// Monthly is the window the dashboard leads with; nil on error.
	Monthly *WindowView `json:"-"`
}

// PoolView aggregates the subscriptions.
//
// The light is the worst of the subscriptions, not an average: quotas are per
// key and cannot be transferred, so the family is fine only if nobody is about
// to hit a ceiling. The periods are also badly out of phase (one at 12% elapsed
// next to one at 76%), which makes an aggregated projection meaningless.
type PoolView struct {
	Level       opencode.Level `json:"level"`
	Total       int            `json:"total"`
	Green       int            `json:"green"`
	Amber       int            `json:"amber"`
	Red         int            `json:"red"`
	Failed      int            `json:"failed"`
	AvgConsumed int            `json:"avgConsumed"`

	// Worst is the subscription closest to trouble, nil if none is measurable.
	Worst *AgentView `json:"-"`
}

// DashboardData is the data passed to the template.
type DashboardData struct {
	Agents   []AgentView
	Pool     PoolView
	Now      time.Time
	KeyCount int
}

func buildDashboardData(statuses []opencode.AgentStatus, now time.Time) DashboardData {
	agents := make([]AgentView, 0, len(statuses))

	for _, s := range statuses {
		av := AgentView{
			Label:     s.Label,
			Error:     s.Error,
			FetchedAt: s.FetchedAt,
			Level:     opencode.LevelGreen,
		}

		for _, w := range s.Windows {
			wv := WindowView{
				Name:     w.Name,
				Kind:     w.Kind,
				Status:   w.Status,
				Percent:  w.Percent,
				ResetsAt: w.ResetsAt,
				ResetIn:  resetIn(w.ResetsAt, now),
				Budget:   opencode.ComputeBudget(w, now),
			}
			// Pace grading where a period start exists; raw consumption on the
			// rolling window, where hitting the ceiling blocks you right now.
			if wv.Budget.Valid {
				wv.Level = wv.Budget.Level
			} else {
				wv.Level = opencode.AbsoluteLevel(w.Percent)
			}
			av.Windows = append(av.Windows, wv)
		}

		// The card badge tracks the monthly window: it is the commitment that
		// matters, and it does not flap like the rolling 5h does.
		for i := range av.Windows {
			if av.Windows[i].Kind == opencode.KindMonthly {
				av.Monthly = &av.Windows[i]
				av.Level = av.Windows[i].Level
				break
			}
		}

		agents = append(agents, av)
	}

	return DashboardData{
		Agents:   agents,
		Pool:     buildPool(agents),
		Now:      now,
		KeyCount: len(agents),
	}
}

func buildPool(agents []AgentView) PoolView {
	pool := PoolView{Level: opencode.LevelGreen, Total: len(agents)}

	sumConsumed, countConsumed := 0, 0
	var worst *AgentView

	for i := range agents {
		a := &agents[i]

		if a.Error != "" {
			pool.Failed++
			continue
		}

		switch a.Level {
		case opencode.LevelRed:
			pool.Red++
		case opencode.LevelAmber:
			pool.Amber++
		default:
			pool.Green++
		}
		pool.Level = opencode.WorstLevel(pool.Level, a.Level)

		if a.Monthly != nil {
			sumConsumed += a.Monthly.Percent
			countConsumed++
			if worst == nil || moreCritical(a, worst) {
				worst = a
			}
		}
	}

	// An unreachable key is an unknown state, not a healthy one. It cannot be
	// told apart from a transient network blip, so it warns rather than alarms.
	if pool.Failed > 0 {
		pool.Level = opencode.WorstLevel(pool.Level, opencode.LevelAmber)
	}

	if countConsumed > 0 {
		pool.AvgConsumed = int(math.Round(float64(sumConsumed) / float64(countConsumed)))
	}
	pool.Worst = worst

	return pool
}

// moreCritical ranks by traffic light first, then by how long the subscription
// would sit at the ceiling, then by raw consumption.
func moreCritical(a, b *AgentView) bool {
	if a.Level != b.Level {
		return opencode.WorstLevel(a.Level, b.Level) == a.Level
	}
	if a.Monthly.Budget.DryDays != b.Monthly.Budget.DryDays {
		return a.Monthly.Budget.DryDays > b.Monthly.Budget.DryDays
	}
	return a.Monthly.Percent > b.Monthly.Percent
}

// Dashboard renders the HTML page.
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	data := buildDashboardData(h.poller.Statuses(), h.now())

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "dashboard.html", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// APIUsage returns JSON usage data, enriched with the budget position.
func (h *Handler) APIUsage(w http.ResponseWriter, r *http.Request) {
	data := buildDashboardData(h.poller.Statuses(), h.now())
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(data.Agents)
}

// Health returns 200 OK.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func levelLabel(l opencode.Level) string {
	switch l {
	case opencode.LevelRed:
		return "dérapage"
	case opencode.LevelAmber:
		return "juste"
	default:
		return "dans le budget"
	}
}

func clamp(v int) int {
	if v > 100 {
		return 100
	}
	if v < 0 {
		return 0
	}
	return v
}

func fmtTime(t time.Time) string {
	return t.In(displayTZ).Format("15:04")
}

// fmtDate renders a reset instant: "22/09 à 03:23".
func fmtDate(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return t.In(displayTZ).Format("02/01 à 15:04")
}

// fmtDay renders a day without the time: "04/09".
func fmtDay(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return t.In(displayTZ).Format("02/01")
}

// resetIn is the time left before a window resets, measured against the same
// instant as the rest of the page rather than against the wall clock.
func resetIn(resetsAt, now time.Time) time.Duration {
	if resetsAt.IsZero() {
		return 0
	}
	if d := resetsAt.Sub(now); d > 0 {
		return d
	}
	return 0
}

func fmtDuration(d time.Duration) string {
	total := int(d.Seconds())
	if total <= 0 {
		return "maintenant"
	}
	days := total / 86400
	hours := (total % 86400) / 3600
	mins := (total % 3600) / 60

	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dj %dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dj", days)
	case hours > 0 && mins > 0:
		return fmt.Sprintf("%dh%02d", hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	case mins > 0:
		return fmt.Sprintf("%dmin", mins)
	default:
		return "< 1min"
	}
}

// fmtDays renders a day count with a French decimal comma: "17,1 j".
func fmtDays(v float64) string {
	if v <= 0 {
		return "0 j"
	}
	return decimal(v, 1) + " j"
}

// fmtRate renders a points-per-day rate: "7,2 pts/j".
func fmtRate(v float64) string {
	return decimal(v, 1) + " pts/j"
}

func decimal(v float64, places int) string {
	return strings.Replace(strconv.FormatFloat(v, 'f', places, 64), ".", ",", 1)
}

// deltaPts renders the gap between consumption and elapsed time: "+15 pts" means
// burning 15 points faster than the period is passing, "-28 pts" means margin.
//
// Plain ASCII on purpose. Headless and minimal browsers have no emoji or
// geometric-shape font, and the traffic light must never depend on one.
// html/template encodes the '+' as &#43;, which browsers display as '+'.
func deltaPts(v int) string {
	switch {
	case v > 0:
		return "+" + strconv.Itoa(v) + " pts"
	case v < 0:
		return "-" + strconv.Itoa(-v) + " pts"
	default:
		return "pile sur le budget"
	}
}
