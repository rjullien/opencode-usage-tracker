package spend

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchSplitsKeysAndLeoUsers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "bifrost_window_cost_usd"):
			fmt.Fprint(w, vector(
				series(12.5, "virtual_key_name", "hermes-lea", "provider", "bedrock", "model", "us.anthropic.claude-sonnet-4-20250514-v1:0"),
				series(136, "virtual_key_name", "hermes-leo", "provider", "opencode-go", "model", "deepseek-v4-flash"),
			))
		case strings.Contains(q, "bifrost_window_tokens"):
			fmt.Fprint(w, vector(
				series(1000, "virtual_key_name", "hermes-lea", "provider", "bedrock", "model", "us.anthropic.claude-sonnet-4-20250514-v1:0", "type", "input"),
				series(10000, "virtual_key_name", "hermes-leo", "provider", "opencode-go", "model", "deepseek-v4-flash", "type", "input"),
				series(9000, "virtual_key_name", "hermes-leo", "provider", "opencode-go", "model", "deepseek-v4-flash", "type", "cache_read"),
				series(50, "virtual_key_name", "hermes-leo", "provider", "opencode-go", "model", "deepseek-v4-flash", "type", "output"),
			))
		case strings.Contains(q, "bifrost_cost_total"):
			fmt.Fprint(w, vector(
				series(2, "virtual_key_name", "tailscale-emma", "provider", "opencode-go", "model", "deepseek-v4.1-flash"),
			))
		case strings.Contains(q, "bifrost_input_tokens_total"):
			fmt.Fprint(w, vector(
				series(500000, "virtual_key_name", "tailscale-emma", "provider", "opencode-go", "model", "deepseek-v4.1-flash"),
			))
		case strings.Contains(q, "bifrost_output_tokens_total"):
			fmt.Fprint(w, vector())
		case strings.Contains(q, "bifrost_cache_read_input_tokens_total"):
			fmt.Fprint(w, vector())
		case strings.Contains(q, `agent="lea"`) && strings.Contains(q, "hermes_window_cost_usd"):
			fmt.Fprint(w, vector(
				series(12.5, "source", "openclaw", "user", "Léa", "provider", "bedrock", "model", "us.anthropic.claude-sonnet-4-20250514-v1:0"),
			))
		case strings.Contains(q, `agent="lea"`) && strings.Contains(q, "hermes_window_tokens"):
			fmt.Fprint(w, vector(
				series(1000, "source", "openclaw", "user", "Léa", "provider", "bedrock", "model", "us.anthropic.claude-sonnet-4-20250514-v1:0", "type", "input"),
			))
		case strings.Contains(q, `agent="lea"`) && strings.Contains(q, "hermes_window_sessions"):
			fmt.Fprint(w, vector(
				series(4, "source", "openclaw", "user", "Léa"),
				series(2, "source", "cron", "user", "aucun"),
			))
		case strings.Contains(q, "hermes_window_cost_usd"):
			fmt.Fprint(w, vector(
				series(135, "source", "telegram", "user", "Camille Jullien", "provider", "opencode-go", "model", "deepseek-v4-flash"),
				series(1000, "source", "telegram", "user", "Camille Jullien"),
				series(49, "source", "telegram", "user", "Baptiste"),
				series(3, "source", "cron", "user", "aucun", "provider", "custom", "model", "deepseek-v4-flash"),
				series(1, "source", "api_server", "user", "aucun", "provider", "opencode-go", "model", "mimo-v2.6-pro"),
			))
		case strings.Contains(q, "hermes_window_tokens"):
			fmt.Fprint(w, vector(
				series(1000, "source", "telegram", "user", "Camille Jullien", "provider", "opencode-go", "model", "deepseek-v4-flash", "type", "input"),
				series(9000, "source", "telegram", "user", "Camille Jullien", "provider", "opencode-go", "model", "deepseek-v4-flash", "type", "cache_read"),
				series(50, "source", "telegram", "user", "Camille Jullien", "provider", "opencode-go", "model", "deepseek-v4-flash", "type", "output"),
				series(10, "source", "cron", "user", "aucun", "provider", "custom", "model", "deepseek-v4-flash", "type", "input"),
				series(40, "source", "cron", "user", "aucun", "provider", "custom", "model", "deepseek-v4-flash", "type", "cache_read"),
			))
		case strings.Contains(q, "hermes_window_sessions"):
			fmt.Fprint(w, vector(
				series(8, "source", "telegram", "user", "Camille Jullien"),
				series(5, "source", "telegram", "user", "Camille Jullien", "provider", "opencode-go", "model", "deepseek-v4-flash"),
				series(170, "source", "cron", "user", "aucun"),
				series(3, "source", "tui", "user", "aucun"),
			))
		default:
			http.Error(w, "unexpected "+q, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	r := NewClient(srv.URL, 2*time.Second).Fetch("1m")
	if r.Error != "" {
		t.Fatalf("error: %s", r.Error)
	}
	if len(r.People) != 3 {
		t.Fatalf("people = %d, want 3 (%+v)", len(r.People), r.People)
	}
	lea := person(r, "hermes-lea")
	if lea.Cost != 12.5 || lea.TokensIn != 1000 || lea.CacheRead != 0 {
		t.Fatalf("lea = %+v", lea)
	}
	if r.From.Day() != 1 || r.From.Month() != time.Now().Month() || r.Window != "1m" {
		t.Fatalf("from = %s window = %s", r.From, r.Window)
	}
	if len(lea.Providers) != 1 || lea.Providers[0].Provider != "bedrock" || lea.Providers[0].Models[0].Model != "us.anthropic.claude-sonnet-4-20250514-v1:0" {
		t.Fatalf("lea tree = %+v", lea.Providers)
	}
	leo := person(r, "hermes-leo")
	// Input from the gauge already includes the cache read.
	if leo.Cost != 136 || leo.TokensIn != 10000 || leo.TokensOut != 50 || leo.CacheRead != 9000 {
		t.Fatalf("leo = %+v", leo.Totals)
	}
	if emma := person(r, "tailscale-emma"); emma.Cost != 2 || emma.TokensIn != 500000 {
		t.Fatalf("emma = %+v", emma)
	}

	if len(r.Leo) != 2 {
		t.Fatalf("leo users = %+v", r.Leo)
	}
	camille := r.Leo[0]
	if camille.User != "Camille Jullien" || camille.CostUSD != 135 || camille.TokensIn != 1000 || camille.TokensOut != 50 || camille.CacheRead != 9000 {
		t.Fatalf("camille = %+v, cache read must stay out of input", camille)
	}
	if camille.Sessions != 8 || len(camille.Providers) != 1 || camille.Providers[0].Provider != "opencode-go" {
		t.Fatalf("camille tree = %+v", camille)
	}
	if camille.Providers[0].Sessions != 5 || len(camille.Providers[0].Models) != 1 || camille.Providers[0].Models[0].Model != "deepseek-v4-flash" || camille.Providers[0].Models[0].Sessions != 5 {
		t.Fatalf("camille model = %+v", camille.Providers)
	}
	if r.Leo[1].User != "Baptiste" || len(r.Leo[1].Providers) != 0 {
		t.Fatalf("second user = %+v", r.Leo[1])
	}
	if r.LeoOther == nil || r.LeoOther.CostUSD != 4 || r.LeoOther.TokensIn != 10 || r.LeoOther.CacheRead != 40 || r.LeoOther.Sessions != 173 {
		t.Fatalf("other = %+v", r.LeoOther)
	}
	if len(r.LeoOther.Providers) != 2 || r.LeoOther.Providers[0].Provider != "custom" || r.LeoOther.Providers[1].Provider != "opencode-go" {
		t.Fatalf("other providers = %+v", r.LeoOther.Providers)
	}
	if r.LeoOther.Providers[0].TokensIn != 10 || r.LeoOther.Providers[0].Models[0].Model != "deepseek-v4-flash" {
		t.Fatalf("other model = %+v", r.LeoOther.Providers[0])
	}
	if len(r.Lea) != 1 || r.Lea[0].User != "Léa" || r.Lea[0].Source != "openclaw" || r.Lea[0].CostUSD != 12.5 || r.Lea[0].Sessions != 4 {
		t.Fatalf("lea = %+v", r.Lea)
	}
	if len(r.Lea[0].Providers) != 1 || r.Lea[0].Providers[0].Provider != "bedrock" || r.Lea[0].Providers[0].Models[0].Model != "us.anthropic.claude-sonnet-4-20250514-v1:0" {
		t.Fatalf("lea tree = %+v", r.Lea[0].Providers)
	}
	if r.LeaOther == nil || r.LeaOther.Sessions != 2 {
		t.Fatalf("lea other = %+v", r.LeaOther)
	}
}

func TestFetchPrometheusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"parse error"}`)
	}))
	defer srv.Close()

	r := NewClient(srv.URL, 2*time.Second).Fetch("1m")
	if r.Error == "" || !strings.Contains(r.Error, "parse error") {
		t.Fatalf("error = %q", r.Error)
	}
	if len(r.People) != 0 || len(r.Leo) != 0 {
		t.Fatalf("partial report on error: %+v", r)
	}
}

func TestFetcherCaches(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, vector())
	}))
	defer srv.Close()

	f := NewFetcher(NewClient(srv.URL, 2*time.Second), time.Minute)
	a := f.Report("1m")
	b := f.Report("1m")
	if a.FetchedAt != b.FetchedAt {
		t.Fatal("second call bypassed the cache")
	}
	if hits != 12 {
		t.Fatalf("hits = %d, want 12 (one scrape of twelve queries)", hits)
	}
	f.Report("3m")
	if hits != 24 {
		t.Fatalf("hits = %d, want 24 (a second window is its own scrape)", hits)
	}
}

func TestWindowBounds(t *testing.T) {
	// 11 Nov 2026 12:00 UTC is 13:00 in Paris, still November.
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id    string
		month time.Month
		year  int
		label string
	}{
		{"1m", time.November, 2026, "1 mois"},
		{"3m", time.September, 2026, "3 mois"},
		{"6m", time.June, 2026, "6 mois"},
		{"1y", time.December, 2025, "1 an"},
		{"nope", time.November, 2026, "1 mois"},
	}
	for _, tc := range cases {
		from, rng, label := windowBounds(now, NormalizeWindow(tc.id))
		from = from.In(paris)
		if from.Day() != 1 || from.Month() != tc.month || from.Year() != tc.year || from.Hour() != 0 {
			t.Errorf("%s from = %s", tc.id, from)
		}
		if label != tc.label {
			t.Errorf("%s label = %s", tc.id, label)
		}
		if !strings.HasSuffix(rng, "s") {
			t.Errorf("%s range = %s", tc.id, rng)
		}
	}
}

func TestMergeBifrostSkipsCopilot(t *testing.T) {
	cost, in, out, cache := mergeBifrost(
		[]sample{},
		[]sample{},
		[]sample{},
		[]sample{},
		[]sample{
			{value: 7, labels: map[string]string{"virtual_key_name": "hermes-leo", "provider": "opencode-go", "model": "deepseek"}},
			{value: 3, labels: map[string]string{"virtual_key_name": "hermes-leo", "provider": "github-copilot", "model": "claude"}},
		},
		[]sample{
			{value: 100, labels: map[string]string{"virtual_key_name": "hermes-leo", "provider": "opencode-go", "model": "deepseek", "type": "input"}},
			{value: 50, labels: map[string]string{"virtual_key_name": "hermes-leo", "provider": "github-copilot", "model": "claude", "type": "input"}},
		},
	)
	if len(cost) != 1 || cost[0].value != 7 || cost[0].labels["provider"] != "opencode-go" {
		t.Fatalf("cost = %+v", cost)
	}
	if len(in) != 1 || in[0].value != 100 {
		t.Fatalf("in = %+v", in)
	}
	if len(out) != 0 || len(cache) != 0 {
		t.Fatalf("out = %+v cache = %+v", out, cache)
	}
}

func TestBifrostFromHermesSkipsCopilot(t *testing.T) {
	row := bifrostFromHermes("hermes-leo", []LeoUser{{
		User: "Camille Jullien",
		LeoTotals: LeoTotals{
			CostUSD: 10, TokensIn: 100, TokensOut: 5, CacheRead: 20,
		},
		Providers: []LeoProvider{
			{
				Provider:  "opencode-go",
				LeoTotals: LeoTotals{CostUSD: 7, TokensIn: 80, TokensOut: 4, CacheRead: 10},
				Models: []LeoModel{{
					Model:     "deepseek-v4-flash",
					LeoTotals: LeoTotals{CostUSD: 7, TokensIn: 80, TokensOut: 4, CacheRead: 10},
				}},
			},
			{
				Provider:  "github-copilot",
				LeoTotals: LeoTotals{CostUSD: 3, TokensIn: 20, TokensOut: 1, CacheRead: 10},
				Models: []LeoModel{{
					Model:     "claude-opus-5",
					LeoTotals: LeoTotals{CostUSD: 3, TokensIn: 20, TokensOut: 1, CacheRead: 10},
				}},
			},
		},
	}}, nil)
	if row.Cost != 7 || row.TokensIn != 90 || row.TokensOut != 4 || row.CacheRead != 10 {
		t.Fatalf("totals = %+v", row.Totals)
	}
	if len(row.Providers) != 1 || row.Providers[0].Provider != "opencode-go" {
		t.Fatalf("providers = %+v", row.Providers)
	}
}

func person(r *Report, name string) PersonSpend {
	for _, p := range r.People {
		if p.VirtualKey == name {
			return p
		}
	}
	return PersonSpend{}
}

func series(v float64, kv ...string) string {
	labels := make([]string, 0, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		labels = append(labels, fmt.Sprintf("%q:%q", kv[i], kv[i+1]))
	}
	return fmt.Sprintf(`{"metric":{%s},"value":[1,"%g"]}`, strings.Join(labels, ","), v)
}

func vector(rows ...string) string {
	return `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(rows, ",") + `]}}`
}
