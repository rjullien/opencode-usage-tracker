package cursor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// APIs Cursor — non documentées (OpenUsage / OpenQuota / AI-Usage).
//
//	POST …/aiserver.v1.DashboardService/GetCurrentPeriodUsage
//	POST …/aiserver.v1.DashboardService/GetPlanInfo            (optionnel)
//	POST …/aiserver.v1.DashboardService/GetSandUsageStatus     (Grok Bot)
//
// Auth : Bearer <access_token> + Connect-Protocol-Version: 1, body {}.
// Repli Grok Bot : POST https://cursor.com/api/dashboard/get-sand-usage-status
// avec cookie WorkosCursorSessionToken=<sub>%3A%3A<access_token>.
//
// Échec soft Grok Bot : les métriques mensuelles Cursor restent affichées.

const (
	defaultAPIBase = "https://api2.cursor.sh"
	defaultWebBase = "https://cursor.com"

	periodUsagePath = "/aiserver.v1.DashboardService/GetCurrentPeriodUsage"
	planInfoPath    = "/aiserver.v1.DashboardService/GetPlanInfo"
	sandUsagePath   = "/aiserver.v1.DashboardService/GetSandUsageStatus"
	sandWebPath     = "/api/dashboard/get-sand-usage-status"
)

// Status porte les données Cursor + Grok Bot affichées par le dashboard.
type Status struct {
	PlanName string `json:"planName,omitempty"`

	// Period : usage mensuel. Nil seulement si le fetch principal a échoué.
	Period *PeriodUsage `json:"period,omitempty"`

	// Grok : allowance hebdo. Nil si pas d'allowance ou fetch soft en échec.
	Grok *GrokUsage `json:"grok,omitempty"`

	// GrokError : diagnostic soft — n'empêche pas d'afficher Period.
	GrokError string `json:"grokError,omitempty"`

	FetchedAt time.Time `json:"fetchedAt"`
	Error     string    `json:"error,omitempty"`
}

// PeriodUsage est le quota mensuel Cursor (GetCurrentPeriodUsage).
type PeriodUsage struct {
	TotalPercent float64    `json:"totalPercent"`
	AutoPercent  float64    `json:"autoPercent"`
	APIPercent   float64    `json:"apiPercent"`
	SpendCents   int64      `json:"spendCents"`
	LimitCents   int64      `json:"limitCents"`
	CycleStart   *time.Time `json:"cycleStart,omitempty"`
	CycleEnd     *time.Time `json:"cycleEnd,omitempty"`
}

// GrokUsage est l'allowance hebdomadaire Grok Bot (GetSandUsageStatus).
type GrokUsage struct {
	UsagePercent float64    `json:"usagePercent"`
	ResetsAt     *time.Time `json:"resetsAt,omitempty"`
}

// Config regroupe ce dont le client a besoin.
type Config struct {
	RefreshToken string
	Timeout      time.Duration
	APIBase      string
	OAuthURL     string
	WebBase      string
}

// ConfigFromEnv lit la config Cursor. Sans CURSOR_REFRESH_TOKEN la section est absente.
func ConfigFromEnv(timeout time.Duration) Config {
	return Config{
		RefreshToken: RefreshTokenFromEnv(),
		Timeout:      timeout,
	}
}

// RefreshTokenFromEnv lit CURSOR_REFRESH_TOKEN (trim : secret K8s / Infisical
// porte souvent un \n final).
func RefreshTokenFromEnv() string {
	return strings.TrimSpace(os.Getenv("CURSOR_REFRESH_TOKEN"))
}

// Client appelle les APIs DashboardService Cursor avec OAuth renouvelable.
type Client struct {
	http *http.Client
	auth *authenticator
	api  string
	web  string
	now  func() time.Time
}

// NewClient crée un client Cursor.
func NewClient(cfg Config) *Client {
	api := cfg.APIBase
	if api == "" {
		api = defaultAPIBase
	}
	web := cfg.WebBase
	if web == "" {
		web = defaultWebBase
	}
	httpClient := &http.Client{Timeout: cfg.Timeout}
	return &Client{
		http: httpClient,
		auth: newAuthenticator(httpClient, cfg.RefreshToken, cfg.OAuthURL),
		api:  strings.TrimRight(api, "/"),
		web:  strings.TrimRight(web, "/"),
		now:  time.Now,
	}
}

// FetchStatus récupère l'usage mensuel Cursor, puis Grok Bot en soft-fail.
func (c *Client) FetchStatus() (Status, error) {
	period, planName, err := c.fetchPeriod()
	if err != nil {
		return Status{}, err
	}
	st := Status{
		PlanName:  planName,
		Period:    &period,
		FetchedAt: c.now(),
	}

	grok, gerr := c.fetchGrok()
	if gerr != nil {
		log.Printf("Cursor: Grok Bot indisponible : %v", gerr)
		st.GrokError = gerr.Error()
	} else if grok != nil {
		st.Grok = grok
	}
	return st, nil
}

func (c *Client) fetchPeriod() (PeriodUsage, string, error) {
	raw, err := c.connectRPC(periodUsagePath, true)
	if err != nil {
		return PeriodUsage{}, "", err
	}
	period, err := parsePeriodUsage(raw)
	if err != nil {
		return PeriodUsage{}, "", err
	}

	planName := ""
	if planRaw, perr := c.connectRPC(planInfoPath, false); perr != nil {
		log.Printf("Cursor: GetPlanInfo indisponible : %v", perr)
	} else {
		planName = parsePlanName(planRaw)
	}
	return period, planName, nil
}

func (c *Client) fetchGrok() (*GrokUsage, error) {
	raw, err := c.connectRPC(sandUsagePath, true)
	if err != nil {
		raw, err = c.fetchGrokWeb()
		if err != nil {
			return nil, err
		}
	}
	return parseGrokUsage(raw)
}

// connectRPC POST body {} avec Bearer + Connect-Protocol-Version: 1.
// retryOn401 : un 401 invalide l'access et retente une fois après refresh.
func (c *Client) connectRPC(path string, retryOn401 bool) ([]byte, error) {
	token, err := c.auth.accessToken()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.api+path, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "opencode-usage-tracker/2.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error (%s): %w", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read error (%s): %w", path, err)
	}

	if resp.StatusCode == http.StatusUnauthorized && retryOn401 {
		c.auth.invalidateAccess()
		return c.connectRPC(path, false)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		log.Printf("Cursor: %s refusé (HTTP %d) — CURSOR_REFRESH_TOKEN %s",
			path, resp.StatusCode, Fingerprint(c.auth.bundle.refresh))
		return nil, &AuthError{
			Msg: fmt.Sprintf(
				"API Cursor refusée (HTTP %d) : session expirée — renouveler CURSOR_REFRESH_TOKEN",
				resp.StatusCode),
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s HTTP %d: %s", path, resp.StatusCode, truncate(string(raw), 200))
	}
	return raw, nil
}

// fetchGrokWeb : repli cookie WorkosCursorSessionToken sur cursor.com.
func (c *Client) fetchGrokWeb() ([]byte, error) {
	token, err := c.auth.accessToken()
	if err != nil {
		return nil, err
	}
	sub, ok := jwtSub(token)
	if !ok {
		return nil, fmt.Errorf("cookie Grok Bot: subject JWT illisible")
	}
	// OpenQuota : userId%3A%3Aaccess_token (seul « :: » est encodé).
	cookie := "WorkosCursorSessionToken=" + sub + "%3A%3A" + token

	req, err := http.NewRequest(http.MethodPost, c.web+sandWebPath, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", c.web)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", "opencode-usage-tracker/2.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error (get-sand-usage-status): %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read error (get-sand-usage-status): %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get-sand-usage-status HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return raw, nil
}

// ---- parsing ----------------------------------------------------------------

type periodUsageWire struct {
	BillingCycleStart string         `json:"billingCycleStart"`
	BillingCycleEnd   string         `json:"billingCycleEnd"`
	PlanUsage         *planUsageWire `json:"planUsage"`
	PlanUsageAlt      *planUsageWire `json:"plan_usage"`
}

type planUsageWire struct {
	TotalPercentUsed json.Number `json:"totalPercentUsed"`
	AutoPercentUsed  json.Number `json:"autoPercentUsed"`
	APIPercentUsed   json.Number `json:"apiPercentUsed"`
	TotalSpend       json.Number `json:"totalSpend"`
	IncludedSpend    json.Number `json:"includedSpend"`
	Limit            json.Number `json:"limit"`
	TotalPercent     json.Number `json:"totalPercent"`
	AutoPercent      json.Number `json:"autoPercent"`
	APIPercent       json.Number `json:"apiPercent"`
	Spend            json.Number `json:"spend"`
}

func parsePeriodUsage(raw []byte) (PeriodUsage, error) {
	var wire periodUsageWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return PeriodUsage{}, fmt.Errorf("JSON parse error (GetCurrentPeriodUsage): %w", err)
	}
	pu := wire.PlanUsage
	if pu == nil {
		pu = wire.PlanUsageAlt
	}
	if pu == nil {
		return PeriodUsage{}, fmt.Errorf("GetCurrentPeriodUsage: planUsage absent")
	}

	out := PeriodUsage{
		TotalPercent: clampPercent(numFloat(firstNumber(pu.TotalPercentUsed, pu.TotalPercent))),
		AutoPercent:  clampPercent(numFloat(firstNumber(pu.AutoPercentUsed, pu.AutoPercent))),
		APIPercent:   clampPercent(numFloat(firstNumber(pu.APIPercentUsed, pu.APIPercent))),
		SpendCents:   numInt(firstNumber(pu.TotalSpend, pu.Spend, pu.IncludedSpend)),
		LimitCents:   numInt(pu.Limit),
	}
	if t, ok := parseTime(wire.BillingCycleStart); ok {
		out.CycleStart = &t
	}
	if t, ok := parseTime(wire.BillingCycleEnd); ok {
		out.CycleEnd = &t
	}
	return out, nil
}

type planInfoWire struct {
	PlanInfo *struct {
		PlanName string `json:"planName"`
	} `json:"planInfo"`
}

func parsePlanName(raw []byte) string {
	var wire planInfoWire
	if err := json.Unmarshal(raw, &wire); err != nil || wire.PlanInfo == nil {
		return ""
	}
	return strings.TrimSpace(wire.PlanInfo.PlanName)
}

type grokUsageWire struct {
	UsagePercent            *float64 `json:"usagePercent"`
	HasNonZeroIncludedLimit *bool    `json:"hasNonZeroIncludedLimit"`
	IncludedLimitZero       *bool    `json:"includedLimitZero"`
	UsesPooledEnterprise    *bool    `json:"usesPooledEnterpriseAllowance"`
	NextResetTimestampUTC   flexTime `json:"nextResetTimestampUtc"`
	NextResetTimestampUTC2  flexTime `json:"nextResetTimestampUTC"`
}

func parseGrokUsage(raw []byte) (*GrokUsage, error) {
	var wire grokUsageWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("JSON parse error (GetSandUsageStatus): %w", err)
	}

	hasLimit := true
	if wire.HasNonZeroIncludedLimit != nil {
		hasLimit = *wire.HasNonZeroIncludedLimit
	}
	if wire.IncludedLimitZero != nil && *wire.IncludedLimitZero {
		hasLimit = false
	}
	if wire.UsesPooledEnterprise != nil && *wire.UsesPooledEnterprise {
		hasLimit = false
	}
	if !hasLimit {
		return nil, nil
	}
	if wire.UsagePercent == nil {
		return nil, nil
	}

	g := &GrokUsage{UsagePercent: clampPercent(*wire.UsagePercent)}
	if t, ok := wire.NextResetTimestampUTC.Time(); ok {
		g.ResetsAt = &t
	} else if t, ok := wire.NextResetTimestampUTC2.Time(); ok {
		g.ResetsAt = &t
	}
	return g, nil
}

// flexTime accepte RFC3339, epoch secondes / ms (nombre ou string).
type flexTime struct {
	t  time.Time
	ok bool
}

func (f *flexTime) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		if t, ok := parseTime(s); ok {
			f.t, f.ok = t, true
			return nil
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			f.t, f.ok = epochToTime(n), true
		}
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return nil
	}
	i, err := n.Int64()
	if err != nil {
		return nil
	}
	f.t, f.ok = epochToTime(i), true
	return nil
}

func (f flexTime) Time() (time.Time, bool) { return f.t, f.ok }

func epochToTime(n int64) time.Time {
	if n > 1_000_000_000_000 {
		return time.UnixMilli(n).UTC()
	}
	return time.Unix(n, 0).UTC()
}

func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func firstNumber(vals ...json.Number) json.Number {
	for _, v := range vals {
		if strings.TrimSpace(v.String()) != "" {
			return v
		}
	}
	return ""
}

func numFloat(n json.Number) float64 {
	s := strings.TrimSpace(n.String())
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

func numInt(n json.Number) int64 {
	s := strings.TrimSpace(n.String())
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(f)
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
