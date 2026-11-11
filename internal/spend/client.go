// Package spend reads per-caller consumption from Prometheus.
//
// Bifrost counters (cost, tokens) are reset when the gateway restarts, so
// those use increase() over the selected span. Hermes window series are
// already the consumption inside that span, computed from session dates, so
// they are read as the current sample. The two numbers are different clocks
// and must not be added together.
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
// Providers is empty when the series have no provider or model label.
type LeoUser struct {
	User   string `json:"user"`
	Source string `json:"source"`
	LeoTotals
	Providers []LeoProvider `json:"providers,omitempty"`
}

// LeoTotals is one Hermes bucket. CacheRead and CacheWrite are disjoint from
// TokensIn: Hermes stores the uncached prompt tail in input.
type LeoTotals struct {
	CostUSD    float64 `json:"costUsd"`
	TokensIn   float64 `json:"tokensIn"`
	TokensOut  float64 `json:"tokensOut"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Sessions   float64 `json:"sessions"`
}

// LeoProvider is one provider under a Hermes person.
type LeoProvider struct {
	Provider string `json:"provider"`
	LeoTotals
	Models []LeoModel `json:"models"`
}

// LeoModel is one model under a Hermes provider.
type LeoModel struct {
	Model string `json:"model"`
	LeoTotals
}

func (t LeoTotals) empty() bool {
	return t.CostUSD == 0 && t.TokensIn == 0 && t.TokensOut == 0 && t.CacheRead == 0 && t.CacheWrite == 0 && t.Sessions == 0
}

// Report is the consumption snapshot. Error set means nothing else is usable:
// a partial table would mix a failed window with a live one.
type Report struct {
	FetchedAt   time.Time     `json:"fetchedAt"`
	From        time.Time     `json:"from"`
	Window      string        `json:"window"`
	WindowLabel string        `json:"windowLabel"`
	Error       string        `json:"error,omitempty"`
	People      []PersonSpend `json:"people"`
	Leo         []LeoUser     `json:"leo"`
	// LeoOther rolls cron, API and TUI into one line. Nil when that remainder is empty.
	LeoOther *LeoUser `json:"leoOther,omitempty"`
	// Lea is Hermes Léa: Telegram plus the imported OpenClaw history.
	// LeaOther rolls the remaining sources (cron) into one line.
	Lea      []LeoUser `json:"lea,omitempty"`
	LeaOther *LeoUser  `json:"leaOther,omitempty"`
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

// Fetch runs the consumption queries for one window. id is 1m, 3m, 6m or 1y;
// anything else is the current calendar month. A transport or query error
// comes back on Report.Error; Fetch itself does not fail, so the dashboard
// can still render.
func (c *Client) Fetch(id string) *Report {
	now := time.Now()
	id = NormalizeWindow(id)
	from, rng, label := windowBounds(now, id)
	hermes := func(metric, agent string) string {
		return dedupe + `(` + metric + `{agent="` + agent + `",window="` + id + `"})`
	}
	queries := []string{
		byCaller + `bifrost_cost_total{virtual_key_name!~"hermes-lea|hermes-leo"}[` + rng + `])))`,
		byCaller + `bifrost_input_tokens_total{virtual_key_name!~"hermes-lea|hermes-leo"}[` + rng + `])))`,
		byCaller + `bifrost_output_tokens_total{virtual_key_name!~"hermes-lea|hermes-leo"}[` + rng + `])))`,
		byCaller + `bifrost_cache_read_input_tokens_total{virtual_key_name!~"hermes-lea|hermes-leo"}[` + rng + `])))`,
		dedupe + `(bifrost_window_cost_usd{window="` + id + `"})`,
		dedupe + `(bifrost_window_tokens{window="` + id + `"})`,
		hermes("hermes_window_cost_usd", "leo"),
		hermes("hermes_window_tokens", "leo"),
		hermes("hermes_window_sessions", "leo"),
		hermes("hermes_window_cost_usd", "lea"),
		hermes("hermes_window_tokens", "lea"),
		hermes("hermes_window_sessions", "lea"),
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
			return &Report{FetchedAt: now, From: from, Window: id, WindowLabel: label, Error: err.Error()}
		}
	}
	cost, in, out, cache := mergeBifrost(samples[0], samples[1], samples[2], samples[3], samples[4], samples[5])
	rep := assemble(now, cost, in, out, cache, samples[6], samples[7], samples[8], samples[9], samples[10], samples[11])
	if !hasVirtualKey(samples[4], "hermes-leo") {
		rep.People = replaceAgentRow(rep.People, "hermes-leo", bifrostFromHermes("hermes-leo", rep.Leo, rep.LeoOther))
	}
	if !hasVirtualKey(samples[4], "hermes-lea") {
		rep.People = replaceAgentRow(rep.People, "hermes-lea", bifrostFromHermes("hermes-lea", rep.Lea, rep.LeaOther))
	}
	sort.Slice(rep.People, func(i, j int) bool {
		return spendLess(rep.People[i].Totals, rep.People[j].Totals, rep.People[i].VirtualKey, rep.People[j].VirtualKey)
	})
	rep.From = from
	rep.Window = id
	rep.WindowLabel = label
	return rep
}

// mergeBifrost appends the window gauges (history from Hermes, Bifrost shape)
// to the live counters. Input, output and cache read arrive on one tokens
// series distinguished by type.
func mergeBifrost(cost, in, out, cache, windowCost, windowTokens []sample) (c, i, o, r []sample) {
	c = append(append([]sample{}, cost...), windowCost...)
	i = append([]sample{}, in...)
	o = append([]sample{}, out...)
	r = append([]sample{}, cache...)
	for _, s := range windowTokens {
		switch s.labels["type"] {
		case "input":
			i = append(i, s)
		case "output":
			o = append(o, s)
		case "cache_read":
			r = append(r, s)
		}
	}
	return c, i, o, r
}

func hasVirtualKey(samples []sample, key string) bool {
	for _, s := range samples {
		if strings.TrimSpace(s.labels["virtual_key_name"]) == key {
			return true
		}
	}
	return false
}

// NormalizeWindow accepts the selector values. Anything else is one month.
func NormalizeWindow(id string) string {
	switch id {
	case "3m", "6m", "1y":
		return id
	default:
		return "1m"
	}
}

// windowBounds is the start of the selected span in Paris, and the Prometheus
// range that reaches back to it. One month is the current calendar month.
// Longer choices walk back whole months and still start on the 1st at 00:00.
// The range is at least five minutes so a query in the first seconds of the
// span still has a window.
func windowBounds(now time.Time, id string) (time.Time, string, string) {
	months, label := 1, "1 mois"
	switch id {
	case "3m":
		months, label = 3, "3 mois"
	case "6m":
		months, label = 6, "6 mois"
	case "1y":
		months, label = 12, "1 an"
	}
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		loc = time.UTC
	}
	n := now.In(loc)
	start := time.Date(n.Year(), n.Month()-time.Month(months-1), 1, 0, 0, 0, 0, loc)
	d := n.Sub(start)
	if d < 5*time.Minute {
		d = 5 * time.Minute
	}
	return start, strconv.Itoa(int(d.Seconds())) + "s", label
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

func assemble(now time.Time, cost, in, out, cache, hCost, hTokens, hSessions, leaCost, leaTokens, leaSessions []sample) *Report {
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

	leo, other := assembleLeo(hCost, hTokens, hSessions, func(source string) bool { return source == "telegram" })
	lea, leaOther := assembleLeo(leaCost, leaTokens, leaSessions, func(source string) bool {
		return source == "telegram" || source == "openclaw"
	})
	return &Report{FetchedAt: now, People: callers, Leo: leo, LeoOther: other, Lea: lea, LeaOther: leaOther}
}

// bifrostFromHermes rolls every person of one agent into the Bifrost table.
// Hermes keeps cache reads out of input; Bifrost counts them inside input, so
// the cached tokens are added back here and the cache column stays the subset.
func bifrostFromHermes(key string, users []LeoUser, other *LeoUser) PersonSpend {
	all := make([]LeoUser, 0, len(users)+1)
	all = append(all, users...)
	if other != nil {
		all = append(all, *other)
	}
	type modelBucket struct{ Totals }
	type providerBucket struct {
		Totals
		models map[string]*modelBucket
	}
	providers := map[string]*providerBucket{}
	var total, covered Totals
	add := func(into *Totals, cost, in, out, cacheRead float64) {
		into.Cost += cost
		into.TokensOut += out
		into.CacheRead += cacheRead
		into.TokensIn += in + cacheRead
	}
	for _, user := range all {
		add(&total, user.CostUSD, user.TokensIn, user.TokensOut, user.CacheRead)
		for _, prov := range user.Providers {
			name := strings.TrimSpace(prov.Provider)
			if name == "" {
				name = "inconnu"
			}
			dst, ok := providers[name]
			if !ok {
				dst = &providerBucket{models: map[string]*modelBucket{}}
				providers[name] = dst
			}
			add(&dst.Totals, prov.CostUSD, prov.TokensIn, prov.TokensOut, prov.CacheRead)
			add(&covered, prov.CostUSD, prov.TokensIn, prov.TokensOut, prov.CacheRead)
			for _, mod := range prov.Models {
				model := strings.TrimSpace(mod.Model)
				if model == "" {
					model = "inconnu"
				}
				mb, ok := dst.models[model]
				if !ok {
					mb = &modelBucket{}
					dst.models[model] = mb
				}
				add(&mb.Totals, mod.CostUSD, mod.TokensIn, mod.TokensOut, mod.CacheRead)
			}
		}
	}
	if gap := total.Cost - covered.Cost; gap > 0.000001 || total.TokensIn-covered.TokensIn > 0.5 {
		dst, ok := providers["inconnu"]
		if !ok {
			dst = &providerBucket{models: map[string]*modelBucket{}}
			providers["inconnu"] = dst
		}
		add(&dst.Totals, total.Cost-covered.Cost, total.TokensIn-covered.TokensIn-(total.CacheRead-covered.CacheRead), total.TokensOut-covered.TokensOut, total.CacheRead-covered.CacheRead)
	}
	row := PersonSpend{VirtualKey: key, Totals: total}
	for name, prov := range providers {
		if prov.zero() {
			continue
		}
		prow := ProviderSpend{Provider: name, Totals: prov.Totals}
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
	return row
}

func replaceAgentRow(callers []PersonSpend, key string, row PersonSpend) []PersonSpend {
	if row.zero() {
		return callers
	}
	for i := range callers {
		if callers[i].VirtualKey == key {
			callers[i] = row
			return callers
		}
	}
	return append(callers, row)
}

// assembleLeo builds one agent's tree. keepSource decides which sources stay
// as their own person; the rest fold into a single "other" line. A sample
// without provider and model stays on the person (the exporter before the
// split, and the per-user session count). A sample with either label also
// feeds that provider and model. Model session counts are not added to the
// person: one session can use several models, and the person total counts
// each session once.
func assembleLeo(hCost, hTokens, hSessions []sample, keepSource func(string) bool) ([]LeoUser, *LeoUser) {
	type modelBucket struct{ LeoTotals }
	type providerBucket struct {
		LeoTotals
		models map[string]*modelBucket
	}
	type personBucket struct {
		source, user string
		LeoTotals
		providers map[string]*providerBucket
	}
	people := map[string]*personBucket{}
	// While Prometheus still has the previous exporter's series (no provider),
	// those would be added on top of the per-model ones. Once a person has a
	// split, the unlabeled cost and tokens are the old sample and are ignored.
	split := map[string]bool{}
	mark := func(s sample) {
		if strings.TrimSpace(s.labels["provider"]) == "" && strings.TrimSpace(s.labels["model"]) == "" {
			return
		}
		source := strings.TrimSpace(s.labels["source"])
		if source == "" {
			source = "unknown"
		}
		split[source+"\x00"+strings.TrimSpace(s.labels["user"])] = true
	}
	for _, s := range hCost {
		mark(s)
	}
	for _, s := range hTokens {
		mark(s)
	}
	person := func(source, user string) *personBucket {
		source = strings.TrimSpace(source)
		user = strings.TrimSpace(user)
		if source == "" {
			source = "unknown"
		}
		id := source + "\x00" + user
		p, ok := people[id]
		if !ok {
			p = &personBucket{source: source, user: user, providers: map[string]*providerBucket{}}
			people[id] = p
		}
		return p
	}
	// rollUp is false for per-model session counts: those must not change the
	// person total, which already counts each session once.
	add := func(s sample, rollUp bool, fn func(*LeoTotals)) {
		provider := strings.TrimSpace(s.labels["provider"])
		model := strings.TrimSpace(s.labels["model"])
		source := strings.TrimSpace(s.labels["source"])
		if source == "" {
			source = "unknown"
		}
		if provider == "" && model == "" && split[source+"\x00"+strings.TrimSpace(s.labels["user"])] {
			return
		}
		p := person(s.labels["source"], s.labels["user"])
		if rollUp {
			fn(&p.LeoTotals)
		}
		if provider == "" && model == "" {
			return
		}
		if provider == "" {
			provider = "inconnu"
		}
		prov, ok := p.providers[provider]
		if !ok {
			prov = &providerBucket{models: map[string]*modelBucket{}}
			p.providers[provider] = prov
		}
		fn(&prov.LeoTotals)
		if model == "" {
			model = "inconnu"
		}
		mod, ok := prov.models[model]
		if !ok {
			mod = &modelBucket{}
			prov.models[model] = mod
		}
		fn(&mod.LeoTotals)
	}
	for _, s := range hCost {
		add(s, true, func(t *LeoTotals) { t.CostUSD += s.value })
	}
	for _, s := range hTokens {
		var fn func(*LeoTotals)
		switch s.labels["type"] {
		case "input":
			fn = func(t *LeoTotals) { t.TokensIn += s.value }
		case "output":
			fn = func(t *LeoTotals) { t.TokensOut += s.value }
		case "cache_read":
			fn = func(t *LeoTotals) { t.CacheRead += s.value }
		case "cache_write":
			fn = func(t *LeoTotals) { t.CacheWrite += s.value }
		default:
			continue
		}
		add(s, true, fn)
	}
	for _, s := range hSessions {
		provider := strings.TrimSpace(s.labels["provider"])
		model := strings.TrimSpace(s.labels["model"])
		if provider == "" && model == "" {
			person(s.labels["source"], s.labels["user"]).Sessions += s.value
			continue
		}
		add(s, false, func(t *LeoTotals) { t.Sessions += s.value })
	}

	finish := func(p *personBucket) LeoUser {
		row := LeoUser{User: p.user, Source: p.source, LeoTotals: p.LeoTotals}
		for provider, prov := range p.providers {
			if prov.empty() {
				continue
			}
			prow := LeoProvider{Provider: provider, LeoTotals: prov.LeoTotals}
			for model, mod := range prov.models {
				if mod.empty() {
					continue
				}
				prow.Models = append(prow.Models, LeoModel{Model: model, LeoTotals: mod.LeoTotals})
			}
			sort.Slice(prow.Models, func(i, j int) bool {
				return leoLess(prow.Models[i].LeoTotals, prow.Models[j].LeoTotals, prow.Models[i].Model, prow.Models[j].Model)
			})
			row.Providers = append(row.Providers, prow)
		}
		sort.Slice(row.Providers, func(i, j int) bool {
			return leoLess(row.Providers[i].LeoTotals, row.Providers[j].LeoTotals, row.Providers[i].Provider, row.Providers[j].Provider)
		})
		return row
	}

	var leo []LeoUser
	var other *personBucket
	for _, p := range people {
		if p.empty() && len(p.providers) == 0 {
			continue
		}
		if keepSource(p.source) {
			leo = append(leo, finish(p))
			continue
		}
		if other == nil {
			other = &personBucket{source: "other", providers: map[string]*providerBucket{}}
		}
		other.LeoTotals.CostUSD += p.CostUSD
		other.TokensIn += p.TokensIn
		other.TokensOut += p.TokensOut
		other.CacheRead += p.CacheRead
		other.CacheWrite += p.CacheWrite
		other.Sessions += p.Sessions
		for provider, prov := range p.providers {
			dst, ok := other.providers[provider]
			if !ok {
				dst = &providerBucket{models: map[string]*modelBucket{}}
				other.providers[provider] = dst
			}
			dst.CostUSD += prov.CostUSD
			dst.TokensIn += prov.TokensIn
			dst.TokensOut += prov.TokensOut
			dst.CacheRead += prov.CacheRead
			dst.CacheWrite += prov.CacheWrite
			dst.Sessions += prov.Sessions
			for model, mod := range prov.models {
				dm, ok := dst.models[model]
				if !ok {
					dm = &modelBucket{}
					dst.models[model] = dm
				}
				dm.CostUSD += mod.CostUSD
				dm.TokensIn += mod.TokensIn
				dm.TokensOut += mod.TokensOut
				dm.CacheRead += mod.CacheRead
				dm.CacheWrite += mod.CacheWrite
				dm.Sessions += mod.Sessions
			}
		}
	}
	sort.Slice(leo, func(i, j int) bool {
		if leo[i].CostUSD != leo[j].CostUSD {
			return leo[i].CostUSD > leo[j].CostUSD
		}
		return leo[i].User < leo[j].User
	})
	if other == nil {
		return leo, nil
	}
	folded := finish(other)
	return leo, &folded
}

func leoLess(a, b LeoTotals, nameA, nameB string) bool {
	if a.CostUSD != b.CostUSD {
		return a.CostUSD > b.CostUSD
	}
	return nameA < nameB
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
