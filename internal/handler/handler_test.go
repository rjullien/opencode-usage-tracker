package handler

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/rjullien/opencode-usage-tracker/internal/opencode"
)

// The instant the reference payload below was captured from the live dashboard.
var captureNow = mustTime("2026-08-25T22:47:54.286Z")

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

// upstreamPayloads replays the exact response the OpenCode API returned for each
// of the four subscriptions at captureNow, in the upstream wire format.
var upstreamPayloads = map[string]string{
	"sk-main": usageJSON(2, "2026-08-26T03:06:28.224Z", 38, "2026-08-31T00:00:00.224Z", 28, "2026-09-22T01:23:30.224Z"),
	"sk-a":    usageJSON(0, "2026-08-26T03:47:54.223Z", 0, "2026-08-31T00:00:00.223Z", 77, "2026-09-03T21:29:30.223Z"),
	"sk-n":    usageJSON(4, "2026-08-26T03:06:46.220Z", 38, "2026-08-31T00:00:00.220Z", 28, "2026-09-21T21:57:17.220Z"),
	"sk-r":    usageJSON(0, "2026-08-26T03:47:54.222Z", 0, "2026-08-31T00:00:00.222Z", 80, "2026-09-02T05:19:30.222Z"),
}

func usageJSON(rollPct int, rollReset string, weekPct int, weekReset string, monthPct int, monthReset string) string {
	return fmt.Sprintf(`{"usage":{
		"rolling":{"status":"ok","percent":%d,"resetsAt":%q},
		"weekly":{"status":"ok","percent":%d,"resetsAt":%q},
		"monthly":{"status":"ok","percent":%d,"resetsAt":%q}
	}}`, rollPct, rollReset, weekPct, weekReset, monthPct, monthReset)
}

// newTestHandler wires the whole chain — HTTP client, fetcher, handler, real
// embedded template — against a stub API, with time frozen at captureNow.
func newTestHandler(t *testing.T) *Handler {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		body, ok := upstreamPayloads[token]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)

	// Same order LoadKeysFromEnv produces, sorted by env var name.
	keys := []opencode.KeyConfig{
		{EnvVar: "OPENCODE_GO_API_KEY", Label: "Main", Key: "sk-main"},
		{EnvVar: "OPENCODE_GO_API_KEY_A", Label: "A", Key: "sk-a"},
		{EnvVar: "OPENCODE_GO_API_KEY_N", Label: "N", Key: "sk-n"},
		{EnvVar: "OPENCODE_GO_API_KEY_R", Label: "R", Key: "sk-r"},
	}

	client := opencode.NewClientWithURL(5*time.Second, srv.URL)
	h := New(opencode.NewFetcher(client, keys, time.Minute))
	h.now = func() time.Time { return captureNow }
	return h
}

func renderDashboard(t *testing.T, h *Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Dashboard(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// mustContain asserts on the page as a browser shows it: html/template encodes
// characters such as '+' into entities, which display as the original character.
func mustContain(t *testing.T, body string, wants ...string) {
	t.Helper()
	visible := html.UnescapeString(body)
	for _, want := range wants {
		if !strings.Contains(visible, want) {
			t.Errorf("rendered page is missing %q", want)
		}
	}
}

// The page must state where each subscription stands versus its own reset.
func TestDashboardRendersPerSubscriptionBudget(t *testing.T) {
	body := renderDashboard(t, newTestHandler(t))

	mustContain(t, body,
		// Main: 28% burned in under 4 days of a 31-day period.
		"13% de la période écoulée",
		"+15 pts",
		"projection 223%",
		"rythme 7,2 pts/j",
		"tenable 2,7 pts/j",
		"17,1 j à sec",
		// A and R are only marginally ahead of budget.
		"2,4 j à sec",
		"1,3 j à sec",
		// The reset instants, in Paris time (UTC+2 in August).
		"22/09 à 03:23",
	)

	// The marker sits at the share of the period elapsed, not at the consumption.
	mustContain(t, body, `style="left: 13%"`, `style="left: 71%"`, `style="left: 77%"`)
	mustContain(t, body, `style="width: 28%"`, `style="width: 77%"`, `style="width: 80%"`)
}

func TestDashboardRendersPoolAsWeakestLink(t *testing.T) {
	body := renderDashboard(t, newTestHandler(t))

	mustContain(t, body,
		// Traffic lights are CSS dots, never emoji: a missing emoji font must not
		// hide the primary signal.
		`<span class="dot-sm red"></span>2 en dérapage`,
		`<span class="dot-sm amber"></span>2 juste`,
		`<span class="dot-sm off"></span>0 dans le budget`,
		"Le plus critique",
		"<strong>Main</strong>",
		"17,1 j à sec",
		// Mean consumption stays as context only.
		"Consommé moyen du parc",
		"<strong>53%</strong>",
		"ne se transfèrent pas",
	)

	// The worst subscription drives the global verdict.
	mustContain(t, body, `<div class="hero red">`, `<span class="dot-lg red"></span>`)

	if strings.ContainsAny(body, "🔴🟠🟢") {
		t.Error("page still relies on emoji for a traffic light")
	}
}

// The rolling window carries no pace maths, so it must not show budget lines.
func TestDashboardRollingHasNoBudgetLines(t *testing.T) {
	body := renderDashboard(t, newTestHandler(t))

	mustContain(t, body, "Rolling 5h", "reset dans 4h18", "reset dans 4h59")

	// Three windows per subscription, but only monthly and weekly are graded on
	// pace: 4 keys x 2 windows = 8 markers, never 12.
	if got, want := strings.Count(body, `class="bar-marker"`), 8; got != want {
		t.Errorf("bar markers = %d, want %d (rolling must not get one)", got, want)
	}
}

func TestDashboardLeadsWithMonthly(t *testing.T) {
	body := renderDashboard(t, newTestHandler(t))

	monthly := strings.Index(body, "Monthly")
	weekly := strings.Index(body, "Weekly")
	rolling := strings.Index(body, "Rolling 5h")

	if monthly < 0 || weekly < 0 || rolling < 0 {
		t.Fatalf("missing a window: monthly=%d weekly=%d rolling=%d", monthly, weekly, rolling)
	}
	if !(monthly < weekly && weekly < rolling) {
		t.Errorf("window order = monthly@%d weekly@%d rolling@%d, want monthly first", monthly, weekly, rolling)
	}

	// The card badge reflects the monthly window, not the flapping rolling one.
	// Main's rolling sits at 2%, so a max-of-windows badge would read green.
	mustContain(t, body, `<span class="badge red"><span class="dot-sm red"></span>dérapage</span>`)
}

func TestAPIUsageExposesBudget(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestHandler(t).APIUsage(rec, httptest.NewRequest(http.MethodGet, "/api/usage", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var agents []AgentView
	if err := json.Unmarshal(rec.Body.Bytes(), &agents); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(agents) != 4 {
		t.Fatalf("agents = %d, want 4", len(agents))
	}

	main := agents[0]
	if main.Label != "Main" {
		t.Fatalf("first agent = %q, want Main", main.Label)
	}
	// The previous field names are preserved for existing consumers.
	if len(main.Windows) != 3 {
		t.Fatalf("windows = %d, want 3", len(main.Windows))
	}
	if main.Level != opencode.LevelRed {
		t.Errorf("Main level = %q, want red", main.Level)
	}

	monthly := main.Windows[0]
	if monthly.Kind != opencode.KindMonthly {
		t.Fatalf("first window kind = %q, want monthly", monthly.Kind)
	}
	if !monthly.Budget.Valid {
		t.Error("monthly budget is not valid")
	}
	if monthly.Budget.ProjectedPct != 223 {
		t.Errorf("projected = %d, want 223", monthly.Budget.ProjectedPct)
	}
	if got := monthly.Budget.DryDays; got < 17.09 || got > 17.11 {
		t.Errorf("dry days = %.3f, want ~17.10", got)
	}

	if rolling := main.Windows[2]; rolling.Budget.Valid {
		t.Error("rolling budget should not be valid")
	}
}

func TestUnreachableKeyWarnsAndIsNamed(t *testing.T) {
	h := newTestHandler(t)
	// Swap in a poller where one subscription failed to answer.
	h.poller = stubPoller{
		{Label: "Main", Error: "clé invalide ou expirée (HTTP 401)", FetchedAt: captureNow},
		// Comfortably inside its budget, so the failed key is the only thing that
		// can move the global verdict.
		{Label: "A", FetchedAt: captureNow, Windows: []opencode.Window{
			{Name: "Monthly", Kind: opencode.KindMonthly, Status: "ok", Percent: 10,
				ResetsAt: mustTime("2026-09-22T01:23:30Z")},
		}},
	}

	body := renderDashboard(t, h)

	mustContain(t, body, "injoignable", "clé invalide ou expirée (HTTP 401)")
	// Unknown is not healthy: the pool must not stay green.
	if strings.Contains(body, `<div class="hero green">`) {
		t.Error("pool is green despite an unreachable key")
	}
	mustContain(t, body, `<div class="hero amber">`)
}

type stubPoller []opencode.AgentStatus

func (s stubPoller) Statuses() []opencode.AgentStatus { return s }

func TestHealth(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestHandler(t).Health(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"ok"}` {
		t.Errorf("body = %q", got)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestHandler(t).Dashboard(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// Nothing on the page may depend on a font shipping arrows, geometric shapes or
// emoji. Minimal and headless browsers draw those as empty boxes, and the traffic
// light is the primary signal of the whole dashboard.
func TestPageAvoidsFontDependentGlyphs(t *testing.T) {
	body := renderDashboard(t, newTestHandler(t))

	// Typographic punctuation carried by every latin font.
	allowed := map[rune]bool{'«': true, '»': true, '·': true, '—': true, '’': true, '•': true}

	seen := map[rune]bool{}
	for _, r := range body {
		if r < 128 || allowed[r] || unicode.IsLetter(r) || seen[r] {
			continue
		}
		seen[r] = true
		t.Errorf("page uses font-dependent glyph %q (U+%04X)", r, r)
	}
}

// Times are displayed in Paris local time, with the real daylight saving offset
// rather than a fixed +1 that is wrong half the year.
//
// This asserts the formatting only. It cannot prove that the timezone database is
// reachable from the scratch runtime image, because the Go toolchain always ships
// $GOROOT/lib/time/zoneinfo.zip and time.LoadLocation finds it during tests even
// with no system tzdata and no time/tzdata import. That guarantee is covered by
// the smoke job in CI, which runs the actual image.
func TestDisplayTimesUseParisLocalTime(t *testing.T) {
	if displayTZ == time.UTC {
		t.Fatal("displayTZ fell back to UTC")
	}

	// 22:47 UTC on 25 August is 00:47 the next day in Paris (CEST, UTC+2).
	if got := fmtTime(captureNow); got != "00:47" {
		t.Errorf("fmtTime = %q, want 00:47 (Europe/Paris, summer time)", got)
	}
	// In January the same zone is UTC+1, which a fixed offset would get wrong one
	// way or the other.
	winter := mustTime("2026-01-15T23:30:00Z")
	if got := fmtTime(winter); got != "00:30" {
		t.Errorf("fmtTime(winter) = %q, want 00:30 (Europe/Paris, winter time)", got)
	}
}
