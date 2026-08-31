package devin

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"time"
)

// Devin CLI/API base URL — endpoint quota ACU : GET /v3/self (Bearer auth).
// NOTE: endpoint non documenté publiquement (l'API Enterprise v2 n'expose que
// des métriques sessions/searches/PRs, PAS le quota ACU). Le chemin /v3/self
// est celui que le CLI Devin utilise (observé dans le binaire 3000.6.7).
const apiBaseURL = "https://api.devin.ai"

// selfPath est le chemin du user status. ASSUMPTION : à vérifier avec un vrai
// token (voir PR). Le CLI l'appelle avec le token du credentials.toml.
const selfPath = "/v3/self"

// PlanInfo est l'ASSUMPTION du payload plan_info renvoyé par /v3/self.
//
// Champs observés dans le binaire devin (strings) — à confirmer avec un vrai
// token avant merge :
//
//	acu_consumed                 — ACU consommés sur la période
//	acu_limit                    — limite d'ACU de la période
//	daily_quota_remaining_percent  — % de quota JOURNALIER restant
//	weekly_quota_remaining_percent — % de quota HEBDO restant
//	top_up_status                — état du top-up automatique
//	grace_period_status          — période de grâce éventuelle
//	plan_start                   — début de la période (ISO 8601)
type PlanInfo struct {
	ACUConsumed             *float64 `json:"acu_consumed"`
	ACULimit                *float64 `json:"acu_limit"`
	DailyQuotaRemainingPct  *float64 `json:"daily_quota_remaining_percent"`
	WeeklyQuotaRemainingPct *float64 `json:"weekly_quota_remaining_percent"`
	ToUpStatus              *string  `json:"top_up_status"`
	GracePeriodStatus       *string  `json:"grace_period_status"`
	PlanStart               *string  `json:"plan_start"`
	DailyQuotaResetAtUnix   *int64   `json:"daily_quota_reset_at_unix"`
	WeeklyQuotaResetAtUnix  *int64   `json:"weekly_quota_reset_at_unix"`
}

// apiResponse est l'ASSUMPTION de la réponse complète de GET /v3/self.
type apiResponse struct {
	UserStatus struct {
		PlanInfo PlanInfo `json:"plan_info"`
	} `json:"user_status"`
}

// Status porte les données ACU Devin affichées par le dashboard.
type Status struct {
	// ACUConsumed/ACULimit : période en cours (mensuelle ou plan).
	ACUConsumed float64 `json:"acuConsumed"`
	ACULimit    float64 `json:"acuLimit"`

	// Percent est la consommation ACU en % de la limite. -1 si non calculable.
	Percent int `json:"percent"`

	// DailyRemainingPct / WeeklyRemainingPct : % de quota RESTANT (100 = neuf).
	// -1 si l'API ne les renvoie pas.
	DailyRemainingPct  int `json:"dailyRemainingPct"`
	WeeklyRemainingPct int `json:"weeklyRemainingPct"`

	// Resets : instants de reset des quotas journalier/hebdo.
	DailyResetAt  time.Time `json:"dailyResetAt,omitempty"`
	WeeklyResetAt time.Time `json:"weeklyResetAt,omitempty"`

	FetchedAt time.Time `json:"fetchedAt"`
	Error     string    `json:"error,omitempty"`
}

// Client appelle l'API Devin.
type Client struct {
	http    *http.Client
	baseURL string
	token   string
}

// NewClient crée un client vers api.devin.ai avec le token donné.
func NewClient(timeout time.Duration, token string) *Client {
	return &Client{
		http:    &http.Client{Timeout: timeout},
		baseURL: apiBaseURL,
		token:   token,
	}
}

// NewClientWithURL crée un client pointant ailleurs (tests).
func NewClientWithURL(timeout time.Duration, token, baseURL string) *Client {
	return &Client{
		http:    &http.Client{Timeout: timeout},
		baseURL: baseURL,
		token:   token,
	}
}

// FetchStatus récupère le statut (quota ACU) du compte Devin.
func (c *Client) FetchStatus() (Status, error) {
	req, err := http.NewRequest("GET", c.baseURL+selfPath, nil)
	if err != nil {
		return Status{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "opencode-usage-tracker/2.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return Status{}, fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Status{}, fmt.Errorf("read error: %w", err)
	}

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return Status{}, fmt.Errorf("token Devin invalide ou expiré (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return Status{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	return parseResponse(body)
}

// parseResponse convertit la réponse API en Status.
//
// ASSUMPTION API : basée sur les champs observés dans le binaire devin
// (strings). À valider avec un vrai token — voir la PR (draft, non mergeable).
func parseResponse(body []byte) (Status, error) {
	var resp apiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Status{}, fmt.Errorf("JSON parse error: %w", err)
	}

	p := resp.UserStatus.PlanInfo
	s := Status{
		FetchedAt:          time.Now(),
		DailyRemainingPct:  -1,
		WeeklyRemainingPct: -1,
	}

	if p.ACUConsumed != nil {
		s.ACUConsumed = *p.ACUConsumed
	}
	if p.ACULimit != nil {
		s.ACULimit = *p.ACULimit
	}
	if p.DailyQuotaRemainingPct != nil {
		s.DailyRemainingPct = int(*p.DailyQuotaRemainingPct)
	}
	if p.WeeklyQuotaRemainingPct != nil {
		s.WeeklyRemainingPct = int(*p.WeeklyQuotaRemainingPct)
	}
	if p.DailyQuotaResetAtUnix != nil {
		s.DailyResetAt = time.Unix(*p.DailyQuotaResetAtUnix, 0)
	}
	if p.WeeklyQuotaResetAtUnix != nil {
		s.WeeklyResetAt = time.Unix(*p.WeeklyQuotaResetAtUnix, 0)
	}

	if s.ACULimit > 0 {
		s.Percent = int(math.Round(s.ACUConsumed / s.ACULimit * 100))
	} else {
		s.Percent = -1
	}

	if s.ACUConsumed == 0 && s.ACULimit == 0 && s.DailyRemainingPct < 0 && s.WeeklyRemainingPct < 0 {
		return Status{}, fmt.Errorf("aucune donnée ACU dans la réponse: %s", truncate(string(body), 300))
	}

	return s, nil
}

// TokenFromEnv lit DEVIN_API_KEY (optionnel : dashboard sans Devin si absent).
func TokenFromEnv() string {
	return os.Getenv("DEVIN_API_KEY")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
