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
				series(50, "virtual_key_name", "hermes-lea", "provider", "bedrock"),
				series(1, "virtual_key_name", "hermes-leo", "provider", "opencode-go"),
				series(2, "virtual_key_name", "tailscale-emma", "provider", "opencode-go"),
			))
		case strings.Contains(q, "bifrost_cost_total[7d]"):
			fmt.Fprint(w, vector(
				series(50, "virtual_key_name", "hermes-lea", "provider", "bedrock"),
				series(3, "virtual_key_name", "hermes-leo", "provider", "opencode-go"),
				series(2, "virtual_key_name", "tailscale-emma", "provider", "opencode-go"),
			))
		case strings.Contains(q, "bifrost_input_tokens_total"):
			fmt.Fprint(w, vector(
				series(11300000, "virtual_key_name", "hermes-lea", "provider", "bedrock"),
				series(2000000, "virtual_key_name", "hermes-leo", "provider", "opencode-go"),
			))
		case strings.Contains(q, "bifrost_output_tokens_total"):
			fmt.Fprint(w, vector(
				series(23000, "virtual_key_name", "hermes-lea", "provider", "bedrock"),
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
				series(9e9, "source", "telegram", "user", "Camille Jullien", "type", "cache_read"),
				series(50, "source", "telegram", "user", "Camille Jullien", "type", "output"),
				series(10, "source", "cron", "user", "aucun", "type", "input"),
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
	if len(r.Keys) != 3 {
		t.Fatalf("keys = %d, want 3 (%+v)", len(r.Keys), r.Keys)
	}
	if r.Keys[0].VirtualKey != "hermes-lea" || r.Keys[0].Provider != "bedrock" || r.Keys[0].Cost24h != 50 {
		t.Fatalf("first key = %+v", r.Keys[0])
	}
	if r.Keys[0].TokensIn7d != 11300000 || r.Keys[0].TokensOut7d != 23000 {
		t.Fatalf("lea tokens = %+v", r.Keys[0])
	}
	leo := key(r, "hermes-leo", "opencode-go")
	if leo.Cost7d != 3 || leo.TokensIn7d != 2000000 || leo.TokensOut7d != 0 {
		t.Fatalf("leo key = %+v", leo)
	}

	if len(r.Leo) != 2 {
		t.Fatalf("leo users = %+v", r.Leo)
	}
	if r.Leo[0].User != "Camille Jullien" || r.Leo[0].TokensIn != 1000 || r.Leo[0].TokensOut != 50 {
		t.Fatalf("camille = %+v, cache read must not count as input", r.Leo[0])
	}
	if r.Leo[1].User != "Baptiste" {
		t.Fatalf("second user = %+v", r.Leo[1])
	}
	if r.LeoOther == nil || r.LeoOther.CostUSD != 4 || r.LeoOther.TokensIn != 10 || r.LeoOther.Sessions != 173 {
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
	if len(r.Keys) != 0 || len(r.Leo) != 0 {
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
	if hits != 7 {
		t.Fatalf("hits = %d, want 7 (one scrape of seven queries)", hits)
	}
}

func key(r *Report, name, provider string) KeySpend {
	for _, k := range r.Keys {
		if k.VirtualKey == name && k.Provider == provider {
			return k
		}
	}
	return KeySpend{}
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
