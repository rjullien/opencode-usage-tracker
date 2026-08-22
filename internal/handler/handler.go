package handler

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/rjullien/opencode-usage-tracker/internal/opencode"
)

// Poller interface for getting statuses.
type Poller interface {
	Statuses() []opencode.AgentStatus
}

// Handler serves the dashboard and API.
type Handler struct {
	tmpl   *template.Template
	poller Poller
}

// New creates a Handler with embedded templates.
func New(fs embed.FS, poller Poller) *Handler {
	funcMap := template.FuncMap{
		"statusColor": statusColor,
		"statusEmoji": statusEmoji,
		"fmtTime":     fmtTime,
		"fmtReset":    fmtReset,
		"maxPercent":  maxPercent,
		"clamp":       func(v int) int { if v > 100 { return 100 }; return v },
	}

	tmpl := template.Must(
		template.New("").Funcs(funcMap).ParseFS(fs, "templates/*.html"),
	)

	return &Handler{
		tmpl:   tmpl,
		poller: poller,
	}
}

// DashboardData is the data passed to the template.
type DashboardData struct {
	Agents        []opencode.AgentStatus
	Now           time.Time
	AvgRolling    int
	AvgWeekly     int
	AvgMonthly    int
	KeyCount      int
}

// Dashboard renders the HTML page.
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	agents := h.poller.Statuses()
	data := DashboardData{
		Agents:   agents,
		Now:      time.Now(),
		KeyCount: len(agents),
	}

	// Compute averages
	var sumR, sumW, sumM int
	var cntR, cntW, cntM int
	for _, a := range agents {
		if a.Error != "" {
			continue
		}
		for _, win := range a.Windows {
			switch win.Name {
			case "Rolling 5h":
				sumR += win.Percent
				cntR++
			case "Weekly":
				sumW += win.Percent
				cntW++
			case "Monthly":
				sumM += win.Percent
				cntM++
			}
		}
	}
	if cntR > 0 {
		data.AvgRolling = sumR / cntR
	}
	if cntW > 0 {
		data.AvgWeekly = sumW / cntW
	}
	if cntM > 0 {
		data.AvgMonthly = sumM / cntM
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "dashboard.html", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// APIUsage returns JSON usage data.
func (h *Handler) APIUsage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(h.poller.Statuses())
}

// Health returns 200 OK.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func statusColor(pct int) string {
	switch {
	case pct >= 90:
		return "red"
	case pct >= 70:
		return "amber"
	default:
		return "green"
	}
}

func statusEmoji(pct int) string {
	switch {
	case pct >= 90:
		return "🔴"
	case pct >= 70:
		return "🟡"
	default:
		return "🟢"
	}
}

func fmtTime(t time.Time) string {
	loc, _ := time.LoadLocation("Europe/Paris")
	if loc == nil {
		loc = time.FixedZone("CET", 3600)
	}
	return t.In(loc).Format("15:04")
}

func fmtReset(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := time.Until(t)
	if d < 0 {
		return "maintenant"
	}
	return fmtDuration(d)
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

func maxPercent(windows []opencode.Window) int {
	max := 0
	for _, w := range windows {
		if w.Percent > max {
			max = w.Percent
		}
	}
	return max
}
