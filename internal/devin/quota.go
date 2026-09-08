package devin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Quotas jour / semaine : l'API REST publique (api.devin.ai) n'expose PAS les
// pourcentages de quota ni les resets journaliers/hebdomadaires. Ils vivent
// dans le Connect-RPC SeatManagementService/GetUserStatus de
// server.codeium.com — le même endpoint que le CLI Devin et OpenUsage.
//
// Constaté en prod (PAT cog_ + credentials.toml) le 08/09/2026 :
//
//	POST /exa.seat_management_pb.SeatManagementService/GetUserStatus
//	body: {"metadata":{"apiKey":"<DEVIN_API_KEY>","ideName":"devin",...}}
//	→ planStatus.dailyQuotaRemainingPercent, dailyQuotaResetAtUnix,
//	  weeklyQuotaResetAtUnix (weeklyQuotaRemainingPercent parfois absent
//	  sur le plan Pro), planInfo.planName, planStart/planEnd.
//
// Le même DEVIN_API_KEY (cog_) fonctionne comme apiKey dans metadata — pas
// besoin d'un second secret windsurf_api_key pour ce chemin.

const (
	defaultSeatBaseURL = "https://server.codeium.com"
	seatUserStatusPath = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"
	seatCompatVersion  = "1.108.2"
)

// Quota porte les pourcentages jour/semaine exposés par GetUserStatus.
// Les pourcentages sont stockés en « used » (100 − remaining), comme le reste
// du dashboard OpenCode : 0 = rien consommé, 100 = plafond.
type Quota struct {
	PlanName string `json:"planName,omitempty"`

	// DailyUsedPercent : nil si le plan masque le quota journalier
	// (hideDailyQuota) ou si le champ est absent de la réponse.
	DailyUsedPercent *float64   `json:"dailyUsedPercent,omitempty"`
	DailyResetsAt    *time.Time `json:"dailyResetsAt,omitempty"`

	// WeeklyUsedPercent : nil quand le plan Pro ne renvoie pas
	// weeklyQuotaRemainingPercent (cas constaté). On n'invente pas de
	// valeur à partir du daily.
	WeeklyUsedPercent *float64   `json:"weeklyUsedPercent,omitempty"`
	WeeklyResetsAt    *time.Time `json:"weeklyResetsAt,omitempty"`

	PlanStart *time.Time `json:"planStart,omitempty"`
	PlanEnd   *time.Time `json:"planEnd,omitempty"`
}

// seatUserStatusResponse est le sous-ensemble utile de GetUserStatus.
type seatUserStatusResponse struct {
	UserStatus struct {
		PlanStatus seatPlanStatus `json:"planStatus"`
	} `json:"userStatus"`
}

type seatPlanStatus struct {
	PlanInfo                    seatPlanInfo `json:"planInfo"`
	PlanStart                   string       `json:"planStart"`
	PlanEnd                     string       `json:"planEnd"`
	DailyQuotaRemainingPercent  *float64     `json:"dailyQuotaRemainingPercent"`
	WeeklyQuotaRemainingPercent *float64     `json:"weeklyQuotaRemainingPercent"`
	DailyQuotaResetAtUnix       json.Number  `json:"dailyQuotaResetAtUnix"`
	WeeklyQuotaResetAtUnix      json.Number  `json:"weeklyQuotaResetAtUnix"`
}

type seatPlanInfo struct {
	PlanName       string `json:"planName"`
	HideDailyQuota bool   `json:"hideDailyQuota"`
}

type seatRequestBody struct {
	Metadata seatMetadata `json:"metadata"`
}

type seatMetadata struct {
	APIKey           string `json:"apiKey"`
	IDEName          string `json:"ideName"`
	IDEVersion       string `json:"ideVersion"`
	ExtensionName    string `json:"extensionName"`
	ExtensionVersion string `json:"extensionVersion"`
	Locale           string `json:"locale"`
}

// fetchQuota interroge GetUserStatus. Échec soft côté FetchStatus : les ACU
// REST restent affichables même si le siège Windsurf est injoignable.
func (c *Client) fetchQuota() (Quota, error) {
	body, err := json.Marshal(seatRequestBody{Metadata: seatMetadata{
		APIKey:           c.token,
		IDEName:          "devin",
		IDEVersion:       seatCompatVersion,
		ExtensionName:    "devin",
		ExtensionVersion: seatCompatVersion,
		Locale:           "en",
	}})
	if err != nil {
		return Quota{}, err
	}

	url := strings.TrimRight(c.seatURL, "/") + seatUserStatusPath
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Quota{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "opencode-usage-tracker/2.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return Quota{}, fmt.Errorf("network error (GetUserStatus): %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Quota{}, fmt.Errorf("read error (GetUserStatus): %w", err)
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		log.Printf("Devin: GetUserStatus refusé (HTTP %d) — DEVIN_API_KEY %s", resp.StatusCode, Fingerprint(c.token))
		return Quota{}, fmt.Errorf("GetUserStatus refusé (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return Quota{}, fmt.Errorf("GetUserStatus HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	return parseQuota(raw)
}

func parseQuota(raw []byte) (Quota, error) {
	var resp seatUserStatusResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Quota{}, fmt.Errorf("JSON parse error (GetUserStatus): %w", err)
	}
	ps := resp.UserStatus.PlanStatus
	if ps.PlanInfo.PlanName == "" && ps.DailyQuotaRemainingPercent == nil && ps.WeeklyQuotaRemainingPercent == nil {
		return Quota{}, fmt.Errorf("GetUserStatus: planStatus vide")
	}

	q := Quota{PlanName: strings.TrimSpace(ps.PlanInfo.PlanName)}
	if t, ok := parseRFC3339(ps.PlanStart); ok {
		q.PlanStart = &t
	}
	if t, ok := parseRFC3339(ps.PlanEnd); ok {
		q.PlanEnd = &t
	}
	if t, ok := unixNumber(ps.DailyQuotaResetAtUnix); ok {
		q.DailyResetsAt = &t
	}
	if t, ok := unixNumber(ps.WeeklyQuotaResetAtUnix); ok {
		q.WeeklyResetsAt = &t
	}

	if !ps.PlanInfo.HideDailyQuota && ps.DailyQuotaRemainingPercent != nil {
		used := clampPercent(100 - *ps.DailyQuotaRemainingPercent)
		q.DailyUsedPercent = &used
	}
	if ps.WeeklyQuotaRemainingPercent != nil {
		used := clampPercent(100 - *ps.WeeklyQuotaRemainingPercent)
		q.WeeklyUsedPercent = &used
	}

	return q, nil
}

func parseRFC3339(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// unixNumber accepte un nombre JSON (1788940800) ou une chaîne ("1788940800"),
// forme constatée dans la réponse GetUserStatus.
func unixNumber(n json.Number) (time.Time, bool) {
	s := strings.TrimSpace(n.String())
	if s == "" {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(s, 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}, false
	}
	return time.Unix(sec, 0).UTC(), true
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
