package bifrost

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// keysPayload mirrors the live response of GET /api/providers/opencode-go/keys
// (captured 2026-08-30): four keys, two with explicit ids, all weights set.
const keysPayload = `{
  "keys": [
    {
      "id": "cface157-ee47-4ee6-87e7-d9491ffa71f7",
      "name": "opencode-go-key-1",
      "value": {"value": "sk-V************************iqyd", "ref": "env.OPENCODE_GO_API_KEY", "type": "env"},
      "models": ["*"], "blacklisted_models": [],
      "weight": 1, "use_for_batch_api": false, "use_anthropic_endpoints": false,
      "status": "success"
    },
    {
      "id": "19a010d1-9132-40ef-a70d-32bd06e32593",
      "name": "opencode-go-key-2",
      "value": {"value": "sk-D************************lmnN", "ref": "env.OPENCODE_GO_API_KEY_R", "type": "env"},
      "models": ["*"], "blacklisted_models": [],
      "weight": 0, "use_for_batch_api": false, "use_anthropic_endpoints": false,
      "status": "success"
    },
    {
      "id": "opencode-go-key-3",
      "name": "opencode-go-key-3",
      "value": {"value": "sk-s************************iLeC", "ref": "env.OPENCODE_GO_API_KEY_A", "type": "env"},
      "models": ["*"], "blacklisted_models": [],
      "weight": 0, "use_for_batch_api": false, "use_anthropic_endpoints": false,
      "status": "success"
    },
    {
      "id": "opencode-go-key-4",
      "name": "opencode-go-key-4",
      "value": {"value": "sk-j************************2B03", "ref": "env.OPENCODE_GO_API_KEY_N", "type": "env"},
      "models": ["*"], "blacklisted_models": [],
      "weight": 1, "use_for_batch_api": false, "use_anthropic_endpoints": false,
      "status": "success"
    }
  ],
  "total": 4
}`

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, 2*time.Second)
}

// The weights are keyed by the environment variable of each key, mapping
// 1:1 onto the dashboard's own key discovery (LoadKeysFromEnv).
func TestWeightsByEnvMapsRefsToEnvVars(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/providers/opencode-go/keys" {
			t.Errorf("path = %q, want the opencode-go keys endpoint", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, keysPayload)
	})

	weights, err := c.WeightsByEnv()
	if err != nil {
		t.Fatalf("WeightsByEnv: %v", err)
	}

	want := map[string]float64{
		"OPENCODE_GO_API_KEY":   1,
		"OPENCODE_GO_API_KEY_R": 0,
		"OPENCODE_GO_API_KEY_A": 0,
		"OPENCODE_GO_API_KEY_N": 1,
	}
	if len(weights) != len(want) {
		t.Fatalf("weights = %v, want %d entries", weights, len(want))
	}
	for env, weight := range want {
		if got := weights[env]; got != weight {
			t.Errorf("weight[%s] = %v, want %v", env, got, weight)
		}
	}
}

// A key whose value does not come from an environment variable has no env
// var to key the weight by and must be skipped, not crash the mapping.
func TestWeightsByEnvSkipsNonEnvRefs(t *testing.T) {
	payload := `{"keys":[
		{"id":"k1","name":"k1","value":{"value":"sk-plain","ref":"","type":"plain_text"},"weight":0.5}
	]}`
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, payload)
	})

	weights, err := c.WeightsByEnv()
	if err != nil {
		t.Fatalf("WeightsByEnv: %v", err)
	}
	if len(weights) != 0 {
		t.Errorf("weights = %v, want empty (no env ref)", weights)
	}
}

func TestWeightsByEnvErrorsAreSurfaced(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	if _, err := c.WeightsByEnv(); err == nil {
		t.Fatal("expected an error on HTTP 500")
	}
}

// A dead Bifrost must fail fast and loudly, never hang the dashboard.
func TestWeightsByEnvNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := NewClient(url, 2*time.Second)
	start := time.Now()
	if _, err := c.WeightsByEnv(); err == nil {
		t.Fatal("expected an error when Bifrost is unreachable")
	}
	if time.Since(start) > 3*time.Second {
		t.Error("network failure took too long")
	}
}
