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
		case strings.Contains(q, "bifrost_cost_total[24h]"):
			fmt.Fprint(w, vector(
				series(50, "virtual_key_name", "hermes-lea", "provider", "bedrock", "model", "claude-opus-5-5"),
				series(1, "virtual_key_name", "hermes-leo", "provider", "opencode-go", "model", "deepseek-v4.1-flash"),
				series(2, "virtual_key_name", "tailscale-emma", "provider", "opencode-go", "model", "deepseek-v4.1-flash"),
			))
		case strings.Contains(q, "bifrost_cost_total[7d]"):
			fmt.Fprint(w, vector(
				series(50, "virtual_key_name", "hermes-lea", "provider", "bedrock", "model", "claude-opus-5-5"),
				series(3, "virtual_key_name", "hermes-leo", "provider", "opencode-go", "model", "deepseek-v4.1-flash"),
				series(2, "virtual_key_name", "tailscale-emma", "provider", "opencode-go", "model", "deepseek-v4.1-flash"),
			))
		case strings.Contains(q, "bifrost_input_tokens_total"):
			fmt.Fprint(w, vector(
				series(11300000, "virtual_key_name", "hermes-lea", "provider", "bedrock", "model", "claude-opus-5-5"),
				series(1500000, "virtual_key_name", "hermes-leo", "provider", "opencode-go", "model", "deepseek-v4.1-flash"),
				series(500000, "virtual_key_name", "hermes-leo", "provider", "opencode-go", "model", "mimo-v2.6-pro"),
			))
		case strings.Contains(q, "bifrost_output_tokens_total"):
			fmt.Fprint(w, vector(
				series(23000, "virtual_key_name", "hermes-lea", "provider", "bedrock", "model", "claude-opus-5-5"),
			))
		case strings.Contains(q, "bifrost_cache_read_input_tokens_total"):
			fmt.Fprint(w, vector(
				series(10800000, "virtual_key_name", "hermes-lea", "provider", "bedrock", "model", "claude-opus-5-5"),
			))
		case strings.Contains(q, "hermes_cost_usd_total"):
			fmt.Fprint(w, vector(
				series(135, "source", "telegram", "user", "Camille Jullien"),
				series(49, "source", "telegram", "user", "Baptiste"),
				series(3, "source", "cron", "user", "aucun"),
				series(1, "source", "api_server", "user", "aucun"),
			))
		case strings.Contains(q, "hermes_tokens_total"):
			fmt.Fprint(w, vector(
				series(1000, "source", "telegram", "user", "Camille Jullien", "type", "input"),
				series(9000, "source", "telegram", "user", "Camille Jullien", "type", "cache_read"),
				series(50, "source", "telegram", "user", "Camille Jullien", "type", "output"),
				series(10, "source", "cron", "user", "aucun", "type", "input"),
				series(40, "source", "cron", "user", "aucun", "type", "cache_read"),
			))
		case strings.Contains(q, "hermes_sessions_total"):
			fmt.Fprint(w, vector(
				series(8, "source", "telegram", "user", "Camille Jullien"),
				series(170, "source", "cron", "user", "aucun"),
				series(3, "source", "tui", "user", "aucun"),
			))
		default:
			http.Error(w, "unexpected "+q, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	r := NewClient(srv.URL, 2*time.Second).Fetch()
	if r.Error != "" {
		t.Fatalf("error: %s", r.Error)
	}
	if len(r.People) != 3 {
		t.Fatalf("people = %d, want 3 (%+v)", len(r.People), r.People)
	}
	lea := r.People[0]
	if lea.VirtualKey != "hermes-lea" || lea.Cost24h != 50 {
		t.Fatalf("first person = %+v", lea)
	}
	if lea.TokensIn7d != 11300000 || lea.TokensOut7d != 23000 || lea.CacheRead7d != 10800000 {
		t.Fatalf("lea tokens = %+v", lea.Totals)
	}
	if len(lea.Providers) != 1 || lea.Providers[0].Provider != "bedrock" || len(lea.Providers[0].Models) != 1 || lea.Providers[0].Models[0].Model != "claude-opus-5-5" {
		t.Fatalf("lea tree = %+v", lea.Providers)
	}
	if lea.Providers[0].CacheRead7d != lea.CacheRead7d || lea.Providers[0].Models[0].CacheRead7d != lea.CacheRead7d {
		t.Fatalf("cache did not roll up: %+v", lea.Providers)
	}
	leo := person(r, "hermes-leo")
	if leo.Cost7d != 3 || leo.TokensIn7d != 2000000 || len(leo.Providers) != 1 || len(leo.Providers[0].Models) != 2 {
		t.Fatalf("leo = %+v models %+v", leo.Totals, leo.Providers)
	}
	if leo.Providers[0].Models[0].Model != "deepseek-v4.1-flash" || leo.Providers[0].Models[1].Model != "mimo-v2.6-pro" {
		t.Fatalf("leo models = %+v", leo.Providers[0].Models)
	}

	if len(r.Leo) != 2 {
		t.Fatalf("leo users = %+v", r.Leo)
	}
	if r.Leo[0].User != "Camille Jullien" || r.Leo[0].TokensIn != 1000 || r.Leo[0].TokensOut != 50 || r.Leo[0].CacheRead != 9000 {
		t.Fatalf("camille = %+v, cache read must stay out of input", r.Leo[0])
	}
	if r.Leo[1].User != "Baptiste" {
		t.Fatalf("second user = %+v", r.Leo[1])
	}
	if r.LeoOther == nil || r.LeoOther.CostUSD != 4 || r.LeoOther.TokensIn != 10 || r.LeoOther.CacheRead != 40 || r.LeoOther.Sessions != 173 {
		t.Fatalf("other = %+v", r.LeoOther)
	}
}

func TestFetchPrometheusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"parse error"}`)
	}))
	defer srv.Close()

	r := NewClient(srv.URL, 2*time.Second).Fetch()
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
	a := f.Report()
	b := f.Report()
	if a.FetchedAt != b.FetchedAt {
		t.Fatal("second call bypassed the cache")
	}
	if hits != 8 {
		t.Fatalf("hits = %d, want 8 (one scrape of eight queries)", hits)
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
