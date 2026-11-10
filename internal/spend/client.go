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

	_ "time/tzdata"
)

const (
	// Drop the scrape identity so two Prometheus replicas of the same series
	// collapse before the sum. Model, method and selected key stay, and the
	// outer sum adds them up per caller.
	dedupe = `max without (prometheus_replica, instance, pod, endpoint, service, namespace, container, job)`

	byCaller = `sum by (virtual_key_name, provider, model) (` + dedupe + `(increase(`

	qHermesCost     = dedupe + `(hermes_cost_usd_total{agent="leo"})`
	qHermesTokens   = dedupe + `(hermes_tokens_total{agent="leo"})`
	qHermesSessions = dedupe + `(hermes_sessions_total{agent="leo"})`
)

// Totals is one consumption bucket over the current calendar month (Paris).
// CacheRead is a subset of TokensIn: Bifrost counts provider prompt tokens in
// input, and the cached ones again here.
type Totals struct {
	Cost      float64 `json:"cost"`
	TokensIn  float64 `json:"tokensIn"`
	TokensOut float64 `json:"tokensOut"`
	CacheRead float64 `json:"cacheRead"`
}

func (t Totals) zero() bool {
	return t.Cost == 0 && t.TokensIn == 0 && t.TokensOut == 0 && t.CacheRead == 0
}

// PersonSpend is one virtual key. Providers and models stay folded until the
// page expands them.
type PersonSpend struct {
	VirtualKey string `json:"virtualKey"`
	Totals
	Providers []ProviderSpend `json:"providers"`
}

// ProviderSpend is one provider under a person.
type ProviderSpend struct {
	Provider string `json:"provider"`
	Totals
	Models []ModelSpend `json:"models"`
}

// ModelSpend is one model under a provider.
type ModelSpend struct {
	Model string `json:"model"`
	Totals
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
	FetchedAt time.Time     `json:"fetchedAt"`
	From      time.Time     `json:"from"`
	Error     string        `json:"error,omitempty"`
	People    []PersonSpend `json:"people"`
	Leo       []LeoUser     `json:"leo"`
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
	from, rng := monthToDate(now)
	queries := []string{
		byCaller + `bifrost_cost_total[` + rng + `])))`,
		byCaller + `bifrost_input_tokens_total[` + rng + `])))`,
		byCaller + `bifrost_output_tokens_total[` + rng + `])))`,
		byCaller + `bifrost_cache_read_input_tokens_total[` + rng + `])))`,
		qHermesCost, qHermesTokens, qHermesSessions,
	}
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
			return &Report{FetchedAt: now, From: from, Error: err.Error()}
		}
	}
	rep := assemble(now, samples[0], samples[1], samples[2], samples[3], samples[4], samples[5], samples[6])
	rep.From = from
	return rep
}

// monthToDate is the current calendar month in Paris, and the Prometheus range
// that reaches back to its first instant. The range is at least five minutes
// so a query in the first seconds of the month still has a window.
func monthToDate(now time.Time) (time.Time, string) {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		loc = time.UTC
	}
	n := now.In(loc)
	start := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc)
	d := n.Sub(start)
	if d < 5*time.Minute {
		d = 5 * time.Minute
	}
	return start, strconv.Itoa(int(d.Seconds())) + "s"
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

func assemble(now time.Time, cost, in, out, cache, hCost, hTokens, hSessions []sample) *Report {
	type modelBucket struct{ Totals }
	type providerBucket struct {
		Totals
		models map[string]*modelBucket
	}
	type personBucket struct {
		Totals
		providers map[string]*providerBucket
	}
	byPerson := map[string]*personBucket{}
	add := func(s sample, fn func(*Totals)) {
		name := strings.TrimSpace(s.labels["virtual_key_name"])
		if name == "" {
			return
		}
		provider := strings.TrimSpace(s.labels["provider"])
		model := strings.TrimSpace(s.labels["model"])
		person, ok := byPerson[name]
		if !ok {
			person = &personBucket{providers: map[string]*providerBucket{}}
			byPerson[name] = person
		}
		fn(&person.Totals)
		prov, ok := person.providers[provider]
		if !ok {
			prov = &providerBucket{models: map[string]*modelBucket{}}
			person.providers[provider] = prov
		}
		fn(&prov.Totals)
		mod, ok := prov.models[model]
		if !ok {
			mod = &modelBucket{}
			prov.models[model] = mod
		}
		fn(&mod.Totals)
	}
	for _, s := range cost {
		add(s, func(t *Totals) { t.Cost += s.value })
	}
	for _, s := range in {
		add(s, func(t *Totals) { t.TokensIn += s.value })
	}
	for _, s := range out {
		add(s, func(t *Totals) { t.TokensOut += s.value })
	}
	for _, s := range cache {
		add(s, func(t *Totals) { t.CacheRead += s.value })
	}

	callers := make([]PersonSpend, 0, len(byPerson))
	for name, person := range byPerson {
		if person.zero() {
			continue
		}
		row := PersonSpend{VirtualKey: name, Totals: person.Totals}
		for provider, prov := range person.providers {
			if prov.zero() {
				continue
			}
			prow := ProviderSpend{Provider: provider, Totals: prov.Totals}
			for model, mod := range prov.models {
				if mod.zero() {
					continue
				}
				prow.Models = append(prow.Models, ModelSpend{Model: model, Totals: mod.Totals})
			}
			sort.Slice(prow.Models, func(i, j int) bool {
				return spendLess(prow.Models[i].Totals, prow.Models[j].Totals, prow.Models[i].Model, prow.Models[j].Model)
			})
			row.Providers = append(row.Providers, prow)
		}
		sort.Slice(row.Providers, func(i, j int) bool {
			return spendLess(row.Providers[i].Totals, row.Providers[j].Totals, row.Providers[i].Provider, row.Providers[j].Provider)
		})
		callers = append(callers, row)
	}
	sort.Slice(callers, func(i, j int) bool {
		return spendLess(callers[i].Totals, callers[j].Totals, callers[i].VirtualKey, callers[j].VirtualKey)
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

	return &Report{FetchedAt: now, People: callers, Leo: leo, LeoOther: other}
}

func spendLess(a, b Totals, nameA, nameB string) bool {
	if a.Cost != b.Cost {
		return a.Cost > b.Cost
	}
	return nameA < nameB
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
