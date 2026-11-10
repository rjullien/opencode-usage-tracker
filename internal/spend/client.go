// Package spend reads per-caller consumption from Prometheus.
//
// Bifrost counters (cost, tokens) are reset when the gateway restarts, so the
// dashboard asks for increase() over a window. Hermes Leo republishes lifetime
// totals from its state database; those are read as the current sample.
// The two numbers are different clocks and must not be added together.
package spend

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Drop the scrape identity so two Prometheus replicas of the same series
	// collapse before the sum. Model, method and selected key stay, and the
	// outer sum adds them up per caller.
	dedupe = `max without (prometheus_replica, instance, pod, endpoint, service, namespace, container, job)`

	qCost24 = `sum by (virtual_key_name, provider) (` + dedupe + `(increase(bifrost_cost_total[24h])))`
	qCost7  = `sum by (virtual_key_name, provider) (` + dedupe + `(increase(bifrost_cost_total[7d])))`
	qIn7    = `sum by (virtual_key_name, provider) (` + dedupe + `(increase(bifrost_input_tokens_total[7d])))`
	qOut7   = `sum by (virtual_key_name, provider) (` + dedupe + `(increase(bifrost_output_tokens_total[7d])))`
	qCache7 = `sum by (virtual_key_name, provider) (` + dedupe + `(increase(bifrost_cache_read_input_tokens_total[7d])))`

	qHermesCost     = dedupe + `(hermes_cost_usd_total{agent="leo"})`
	qHermesTokens   = dedupe + `(hermes_tokens_total{agent="leo"})`
	qHermesSessions = dedupe + `(hermes_sessions_total{agent="leo"})`
)

// KeySpend is one Bifrost virtual key on one provider.
type KeySpend struct {
	VirtualKey  string  `json:"virtualKey"`
	Provider    string  `json:"provider"`
	Cost24h     float64 `json:"cost24h"`
	Cost7d      float64 `json:"cost7d"`
	TokensIn7d  float64 `json:"tokensIn7d"`
	TokensOut7d float64 `json:"tokensOut7d"`
	// CacheRead7d is a subset of TokensIn7d: Bifrost counts provider prompt
	// tokens in input, and the cached ones again here.
	CacheRead7d float64 `json:"cacheRead7d"`
}

// LeoUser is one person (or the rolled-up non-Telegram remainder) inside Hermes Leo.
type LeoUser struct {
	User      string  `json:"user"`
	Source    string  `json:"source"`
	CostUSD   float64 `json:"costUsd"`
	TokensIn  float64 `json:"tokensIn"`
	TokensOut float64 `json:"tokensOut"`
	// CacheRead and CacheWrite are disjoint from TokensIn. Hermes stores the
	// uncached prompt tail in input and the cached prefix beside it.
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Sessions   float64 `json:"sessions"`
}

// Report is the consumption snapshot. Error set means nothing else is usable:
// a partial table would mix a failed window with a live one.
type Report struct {
	FetchedAt time.Time  `json:"fetchedAt"`
	Error     string     `json:"error,omitempty"`
	Keys      []KeySpend `json:"keys"`
	Leo       []LeoUser  `json:"leo"`
	// LeoOther rolls cron, API and TUI into one line. Nil when that remainder is empty.
	LeoOther *LeoUser `json:"leoOther,omitempty"`
}

// Client queries the Prometheus HTTP API.
type Client struct {
	http    *http.Client
	baseURL string
}

// NewClient creates a Client. baseURL is the Prometheus origin, without /api/v1.
func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		http:    &http.Client{Timeout: timeout},
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
	}
}

// Fetch runs the consumption queries. A transport or query error comes back on
// Report.Error; Fetch itself does not fail, so the dashboard can still render.
func (c *Client) Fetch() *Report {
	now := time.Now()
	queries := []string{qCost24, qCost7, qIn7, qOut7, qCache7, qHermesCost, qHermesTokens, qHermesSessions}
	samples := make([][]sample, len(queries))
	errs := make([]error, len(queries))

	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			samples[i], errs[i] = c.query(q)
		}(i, q)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return &Report{FetchedAt: now, Error: err.Error()}
		}
	}
	return assemble(now, samples[0], samples[1], samples[2], samples[3], samples[4], samples[5], samples[6], samples[7])
}

type sample struct {
	labels map[string]string
	value  float64
}

func (c *Client) query(promql string) ([]sample, error) {
	u, err := url.Parse(c.baseURL + "/api/v1/query")
	if err != nil {
		return nil, fmt.Errorf("prometheus: url invalide")
	}
	qv := u.Query()
	qv.Set("query", promql)
	u.RawQuery = qv.Encode()

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("prometheus: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus injoignable: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("prometheus: lecture: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus HTTP %d: %s", resp.StatusCode, truncate(string(body), 180))
	}

	var parsed promResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("prometheus: réponse invalide")
	}
	if parsed.Status != "success" {
		msg := parsed.Error
		if msg == "" {
			msg = parsed.Status
		}
		return nil, fmt.Errorf("prometheus: %s", truncate(msg, 180))
	}
	if parsed.Data.ResultType != "vector" && parsed.Data.ResultType != "" {
		return nil, fmt.Errorf("prometheus: résultat %s inattendu", parsed.Data.ResultType)
	}

	out := make([]sample, 0, len(parsed.Data.Result))
	for _, row := range parsed.Data.Result {
		v, ok := row.value()
		if !ok {
			continue
		}
		out = append(out, sample{labels: row.Metric, value: v})
	}
	return out, nil
}

type promResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string       `json:"resultType"`
		Result     []promSample `json:"result"`
	} `json:"data"`
}

type promSample struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}

func (s promSample) value() (float64, bool) {
	if len(s.Value) != 2 {
		return 0, false
	}
	raw, ok := s.Value[1].(string)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	if v < 0 {
		return 0, true
	}
	return v, true
}

func assemble(now time.Time, cost24, cost7, in7, out7, cache7, hCost, hTokens, hSessions []sample) *Report {
	type acc struct {
		key KeySpend
	}
	keys := map[string]*acc{}
	touch := func(name, provider string) *acc {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil
		}
		provider = strings.TrimSpace(provider)
		id := name + "\x00" + provider
		a, ok := keys[id]
		if !ok {
			a = &acc{key: KeySpend{VirtualKey: name, Provider: provider}}
			keys[id] = a
		}
		return a
	}
	for _, s := range cost24 {
		if a := touch(s.labels["virtual_key_name"], s.labels["provider"]); a != nil {
			a.key.Cost24h += s.value
		}
	}
	for _, s := range cost7 {
		if a := touch(s.labels["virtual_key_name"], s.labels["provider"]); a != nil {
			a.key.Cost7d += s.value
		}
	}
	for _, s := range in7 {
		if a := touch(s.labels["virtual_key_name"], s.labels["provider"]); a != nil {
			a.key.TokensIn7d += s.value
		}
	}
	for _, s := range out7 {
		if a := touch(s.labels["virtual_key_name"], s.labels["provider"]); a != nil {
			a.key.TokensOut7d += s.value
		}
	}
	for _, s := range cache7 {
		if a := touch(s.labels["virtual_key_name"], s.labels["provider"]); a != nil {
			a.key.CacheRead7d += s.value
		}
	}

	rows := make([]KeySpend, 0, len(keys))
	for _, a := range keys {
		rows = append(rows, a.key)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Cost24h != rows[j].Cost24h {
			return rows[i].Cost24h > rows[j].Cost24h
		}
		if rows[i].Cost7d != rows[j].Cost7d {
			return rows[i].Cost7d > rows[j].Cost7d
		}
		if rows[i].VirtualKey != rows[j].VirtualKey {
			return rows[i].VirtualKey < rows[j].VirtualKey
		}
		return rows[i].Provider < rows[j].Provider
	})

	people := map[string]*LeoUser{}
	person := func(source, user string) *LeoUser {
		source = strings.TrimSpace(source)
		user = strings.TrimSpace(user)
		if source == "" {
			source = "unknown"
		}
		id := source + "\x00" + user
		p, ok := people[id]
		if !ok {
			p = &LeoUser{Source: source, User: user}
			people[id] = p
		}
		return p
	}
	for _, s := range hCost {
		person(s.labels["source"], s.labels["user"]).CostUSD += s.value
	}
	for _, s := range hTokens {
		p := person(s.labels["source"], s.labels["user"])
		switch s.labels["type"] {
		case "input":
			p.TokensIn += s.value
		case "output":
			p.TokensOut += s.value
		case "cache_read":
			p.CacheRead += s.value
		case "cache_write":
			p.CacheWrite += s.value
		}
	}
	for _, s := range hSessions {
		person(s.labels["source"], s.labels["user"]).Sessions += s.value
	}

	var leo []LeoUser
	var other *LeoUser
	for _, p := range people {
		if p.CostUSD == 0 && p.TokensIn == 0 && p.TokensOut == 0 && p.CacheRead == 0 && p.CacheWrite == 0 && p.Sessions == 0 {
			continue
		}
		if p.Source == "telegram" {
			leo = append(leo, *p)
			continue
		}
		if other == nil {
			other = &LeoUser{Source: "other"}
		}
		other.CostUSD += p.CostUSD
		other.TokensIn += p.TokensIn
		other.TokensOut += p.TokensOut
		other.CacheRead += p.CacheRead
		other.CacheWrite += p.CacheWrite
		other.Sessions += p.Sessions
	}
	sort.Slice(leo, func(i, j int) bool {
		if leo[i].CostUSD != leo[j].CostUSD {
			return leo[i].CostUSD > leo[j].CostUSD
		}
		return leo[i].User < leo[j].User
	})

	return &Report{FetchedAt: now, Keys: rows, Leo: leo, LeoOther: other}
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
