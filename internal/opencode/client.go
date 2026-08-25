package opencode

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const apiURL = "https://opencode.ai/zen/go/v1/usage"

// Window kinds. Each kind has its own period-start rule, verified against real
// API responses:
//
//	rolling — genuinely rolling; with zero usage the API returns now+5h, so no
//	          period start can be derived.
//	weekly  — calendar-aligned: every key resets at the same Monday 00:00 UTC.
//	monthly — subscription anniversary: arbitrary day-of-month and time-of-day,
//	          different for every key.
const (
	KindRolling = "rolling"
	KindWeekly  = "weekly"
	KindMonthly = "monthly"
)

// Window represents a single quota window (rolling, weekly, monthly).
type Window struct {
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Status   string    `json:"status"`
	Percent  int       `json:"percent"`
	ResetsAt time.Time `json:"resetsAt"`
}

// apiResponse matches the real API response format.
type apiResponse struct {
	Usage struct {
		Rolling *windowRaw `json:"rolling"`
		Weekly  *windowRaw `json:"weekly"`
		Monthly *windowRaw `json:"monthly"`
	} `json:"usage"`
}

type windowRaw struct {
	Status   string `json:"status"`
	Percent  int    `json:"percent"`
	ResetsAt string `json:"resetsAt"`
}

// Client calls the OpenCode Go usage API.
type Client struct {
	http    *http.Client
	baseURL string
}

// NewClient creates a Client with the given timeout.
func NewClient(timeout time.Duration) *Client {
	return &Client{
		http:    &http.Client{Timeout: timeout},
		baseURL: apiURL,
	}
}

// NewClientWithURL creates a Client pointing at a custom base URL (for testing).
func NewClientWithURL(timeout time.Duration, baseURL string) *Client {
	return &Client{
		http:    &http.Client{Timeout: timeout},
		baseURL: baseURL,
	}
}

// FetchUsage retrieves usage data for the given API key.
func (c *Client) FetchUsage(apiKey string) ([]Window, error) {
	req, err := http.NewRequest("GET", c.baseURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
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
		return nil, fmt.Errorf("clé invalide ou expirée (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("rate-limited (HTTP 429)")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	return parseResponse(body)
}

func parseResponse(body []byte) ([]Window, error) {
	var resp apiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("JSON parse error: %w", err)
	}

	var windows []Window

	// Monthly first: it is the window that actually constrains the month, and
	// the one the dashboard leads with.
	if w := resp.Usage.Monthly; w != nil {
		windows = append(windows, toWindow("Monthly", KindMonthly, w))
	}
	if w := resp.Usage.Weekly; w != nil {
		windows = append(windows, toWindow("Weekly", KindWeekly, w))
	}
	if w := resp.Usage.Rolling; w != nil {
		windows = append(windows, toWindow("Rolling 5h", KindRolling, w))
	}

	if len(windows) == 0 {
		return nil, fmt.Errorf("aucune fenêtre trouvée dans la réponse: %s", truncate(string(body), 300))
	}

	return windows, nil
}

func toWindow(name, kind string, raw *windowRaw) Window {
	w := Window{
		Name:    name,
		Kind:    kind,
		Status:  raw.Status,
		Percent: raw.Percent,
	}
	if t, err := time.Parse(time.RFC3339Nano, raw.ResetsAt); err == nil {
		w.ResetsAt = t
	} else if t, err := time.Parse(time.RFC3339, raw.ResetsAt); err == nil {
		w.ResetsAt = t
	}
	return w
}

// ResetIn returns the duration until the window resets.
func (w Window) ResetIn() time.Duration {
	if w.ResetsAt.IsZero() {
		return 0
	}
	d := time.Until(w.ResetsAt)
	if d < 0 {
		return 0
	}
	return d
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
