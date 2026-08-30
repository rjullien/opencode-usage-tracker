// Package bifrost reads routing state from the Bifrost gateway management API.
//
// The dashboard only ever reads weights: changing routing stays a deliberate
// act performed through Bifrost itself (UI or PUT /api/providers/.../keys).
package bifrost

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// provider is the only provider this dashboard cares about.
const provider = "opencode-go"

// Key is one provider key as exposed by Bifrost's management API
// (GET /api/providers/{provider}/keys).
type Key struct {
	ID     string    `json:"id"`
	Name   string    `json:"name"`
	Value  SecretRef `json:"value"`
	Weight float64   `json:"weight"`
}

// SecretRef carries the source reference of a key value ("env.VAR"), never
// the resolved secret itself.
type SecretRef struct {
	Value string `json:"value,omitempty"`
	Ref   string `json:"ref,omitempty"`
	Type  string `json:"type,omitempty"`
}

// keysResponse matches the envelope of the Bifrost keys endpoint.
type keysResponse struct {
	Keys []Key `json:"keys"`
}

// Client fetches provider key state from a Bifrost instance.
type Client struct {
	http    *http.Client
	baseURL string
}

// NewClient creates a Client for the given base URL (e.g.
// http://bifrost.openclaw.svc.cluster.local:8080). Bifrost is an auxiliary
// source: a slow or dead gateway must not block the dashboard for long, so
// keep the timeout short.
func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		http:    &http.Client{Timeout: timeout},
		baseURL: strings.TrimRight(baseURL, "/"),
	}
}

// WeightsByEnv returns the current load-balancing weight of every key of the
// opencode-go provider, keyed by the environment variable that holds the key
// (the "env." prefix of the key's value ref is stripped). Keys without an env
// ref are skipped. A non-nil error means no weight could be fetched at all.
func (c *Client) WeightsByEnv() (map[string]float64, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/api/providers/"+provider+"/keys", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bifrost injoignable: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("lecture réponse bifrost: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bifrost HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var parsed keysResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("réponse bifrost invalide: %w", err)
	}

	weights := make(map[string]float64, len(parsed.Keys))
	for _, k := range parsed.Keys {
		ref := k.Value.Ref
		if !strings.HasPrefix(ref, "env.") {
			continue
		}
		weights[strings.TrimPrefix(ref, "env.")] = k.Weight
	}
	return weights, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
