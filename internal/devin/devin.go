package devin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

// API Devin — endpoints documentés (v3, token PAT ou service user `cog_`) :
//
//	GET /v3/self                             → identité + org_id (200)
//	GET /v3/organizations/{org}/consumption/daily → consommation ACU (200)
//
// Schéma réel vérifié le 31/08/2026 avec un PAT `cog_` :
//
//	GET /v3/self →
//	  {"principal_type":"pat_user","user_id":"user-...","org_id":"org-..."}
//
//	GET /v3/organizations/{org}/consumption/daily →
//	  {"total_acus":0.0,"consumption_by_date":[
//	    {"date":1788393600,"acus":12.5,
//	     "acus_by_product":{"devin":12.5,"cascade":0,"terminal":0,"review":null}}
//	  ]}
//
// ⚠️ La LIMITE ACU (`acu_limit`, `daily_quota_remaining_percent`...) n'est PAS
// exposée par l'API REST publique : elle ne vit que dans le gRPC interne du CLI
// (GetUserStatus). Le board affiche donc la CONSOMMATION ACU réelle.

const apiBaseURL = "https://api.devin.ai"

const (
	selfPath        = "/v3/self"
	consumptionPath = "/v3/organizations/%s/consumption/daily"
)

// SelfResponse est la réponse de GET /v3/self.
type SelfResponse struct {
	PrincipalType string `json:"principal_type"`
	UserID        string `json:"user_id"`
	UserName      string `json:"user_name"`
	OrgID         string `json:"org_id"`
}

// ConsumptionResponse est la réponse de
// GET /v3/organizations/{org}/consumption/daily.
type ConsumptionResponse struct {
	TotalACUs         float64             `json:"total_acus"`
	ConsumptionByDate []ConsumptionByDate `json:"consumption_by_date"`
}

// ConsumptionByDate est un jour de consommation.
type ConsumptionByDate struct {
	Date          int64         `json:"date"`
	ACUs          float64       `json:"acus"`
	ACUsByProduct ACUsByProduct `json:"acus_by_product"`
}

// ACUsByProduct détaille la consommation par produit Devin.
type ACUsByProduct struct {
	Devin    float64  `json:"devin"`
	Cascade  float64  `json:"cascade"`
	Terminal float64  `json:"terminal"`
	Review   *float64 `json:"review"`
}

// Status porte les données ACU Devin affichées par le dashboard.
//
// Le dashboard montre la CONSOMMATION réelle (total_acus de la période) et sa
// répartition par produit. Pas de pourcentage : la limite ACU du plan n'est
// pas exposée par l'API publique.
type Status struct {
	// ACUConsumed : total ACU consommés sur la période couverte par l'API.
	ACUConsumed float64 `json:"acuConsumed"`

	// Days : consommation par jour (date epoch, acus, acusByProduct).
	Days []DayUsage `json:"days,omitempty"`

	// DayCount : nombre de jours de consommation renvoyés.
	DayCount int `json:"dayCount"`

	// OrgID : organisation du compte (via /v3/self).
	OrgID string `json:"orgId,omitempty"`

	FetchedAt time.Time `json:"fetchedAt"`
	Error     string    `json:"error,omitempty"`
}

// DayUsage est un jour de consommation ACU.
type DayUsage struct {
	Date          time.Time     `json:"date"`
	ACUs          float64       `json:"acus"`
	ACUsByProduct ACUsByProduct `json:"acusByProduct"`
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

// FetchStatus récupère la consommation ACU Devin : d'abord /v3/self pour
// l'org_id, puis l'endpoint consumption/daily de l'organisation.
func (c *Client) FetchStatus() (Status, error) {
	self, err := c.getJSON(selfPath)
	if err != nil {
		return Status{}, err
	}
	var sr SelfResponse
	if err := json.Unmarshal(self, &sr); err != nil {
		return Status{}, fmt.Errorf("JSON parse error (/v3/self): %w", err)
	}
	if sr.OrgID == "" {
		return Status{}, fmt.Errorf("aucun org_id dans /v3/self")
	}

	body, err := c.getJSON(fmt.Sprintf(consumptionPath, sr.OrgID))
	if err != nil {
		return Status{}, err
	}
	return parseResponse(body, sr.OrgID)
}

func (c *Client) getJSON(path string) ([]byte, error) {
	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "opencode-usage-tracker/2.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read error: %w", err)
	}

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("token Devin invalide ou expiré (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

// parseResponse convertit la réponse consumption/daily en Status.
func parseResponse(body []byte, orgID string) (Status, error) {
	var resp ConsumptionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Status{}, fmt.Errorf("JSON parse error: %w", err)
	}

	s := Status{
		ACUConsumed: resp.TotalACUs,
		OrgID:       orgID,
		FetchedAt:   time.Now(),
	}

	for _, d := range resp.ConsumptionByDate {
		day := DayUsage{
			Date:          time.Unix(d.Date, 0),
			ACUs:          d.ACUs,
			ACUsByProduct: d.ACUsByProduct,
		}
		s.Days = append(s.Days, day)
	}
	s.DayCount = len(s.Days)

	return s, nil
}

// TokenFromEnv lit DEVIN_API_KEY (optionnel : dashboard sans Devin si absent).
func TokenFromEnv() string {
	return os.Getenv("DEVIN_API_KEY")
}

// ResetDayFromEnv lit DEVIN_RESET_DAY : jour du mois du reset de budget Devin.
// Défaut 5 (le plan Pro individuel reset le 5 du mois — valeur constatée dans
// la UI, non exposée par l'API REST). Clampé à [1,28] pour éviter les mois
// sans ce jour.
func ResetDayFromEnv() int {
	day := 5
	if v := os.Getenv("DEVIN_RESET_DAY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 28 {
			day = n
		}
	}
	return day
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
