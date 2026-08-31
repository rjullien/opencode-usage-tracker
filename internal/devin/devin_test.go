package devin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testServer renvoie un serveur HTTP qui répond exactement comme l'API Devin
// le fera (assumption). token attendu : "test-token".
func testServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/self" {
			http.Error(w, "wrong path "+r.URL.Path, 404)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			http.Error(w, "bad auth: "+got, 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const sampleBody = `{
  "user_status": {
    "plan_info": {
      "acu_consumed": 42.5,
      "acu_limit": 500,
      "daily_quota_remaining_percent": 61.2,
      "weekly_quota_remaining_percent": 34.8,
      "top_up_status": "none",
      "plan_start": "2026-08-01T00:00:00Z",
      "daily_quota_reset_at_unix": 1788393600,
      "weekly_quota_reset_at_unix": 1788393600
    }
  }
}`

func TestParseResponse(t *testing.T) {
	s, err := parseResponse([]byte(sampleBody))
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if s.ACUConsumed != 42.5 {
		t.Errorf("ACUConsumed = %v, want 42.5", s.ACUConsumed)
	}
	if s.ACULimit != 500 {
		t.Errorf("ACULimit = %v, want 500", s.ACULimit)
	}
	if s.Percent != 9 {
		t.Errorf("Percent = %d, want 9 (42.5/500)", s.Percent)
	}
	if s.DailyRemainingPct != 61 || s.WeeklyRemainingPct != 34 {
		t.Errorf("remaining pct = %d/%d, want 61/34", s.DailyRemainingPct, s.WeeklyRemainingPct)
	}
	if s.DailyResetAt.IsZero() {
		t.Error("DailyResetAt should be set")
	}
	if s.Error != "" {
		t.Errorf("unexpected error: %s", s.Error)
	}
}

func TestParseResponseEmpty(t *testing.T) {
	_, err := parseResponse([]byte(`{"user_status": {}}`))
	if err == nil {
		t.Fatal("expected error on empty payload")
	}
	if !strings.Contains(err.Error(), "aucune donnée ACU") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseResponseMalformed(t *testing.T) {
	_, err := parseResponse([]byte(`{not json`))
	if err == nil {
		t.Fatal("expected JSON parse error")
	}
}

func TestFetchStatusOK(t *testing.T) {
	srv := testServer(t, sampleBody, 200)
	c := NewClientWithURL(2*time.Second, "test-token", srv.URL)
	s, err := c.FetchStatus()
	if err != nil {
		t.Fatalf("FetchStatus: %v", err)
	}
	if s.Percent != 9 {
		t.Errorf("Percent = %d, want 9", s.Percent)
	}
	if s.FetchedAt.IsZero() {
		t.Error("FetchedAt should be set")
	}
}

func TestFetchStatusBadToken(t *testing.T) {
	srv := testServer(t, `{"error":"unauthorized"}`, 401)
	c := NewClientWithURL(2*time.Second, "test-token", srv.URL)
	_, err := c.FetchStatus()
	if err == nil {
		t.Fatal("expected auth error")
	}
	if !strings.Contains(err.Error(), "invalide ou expiré") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFetchStatusServerError(t *testing.T) {
	srv := testServer(t, `{"error":"boom"}`, 500)
	c := NewClientWithURL(2*time.Second, "test-token", srv.URL)
	_, err := c.FetchStatus()
	if err == nil {
		t.Fatal("expected server error")
	}
}

func TestFetcherNoToken(t *testing.T) {
	f := NewFetcher(nil, "", time.Minute)
	if s := f.Statuses(); s != nil {
		t.Fatalf("expected nil status without token, got %+v", s)
	}
}

func TestFetcherCache(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(sampleBody))
	}))
	t.Cleanup(srv.Close)

	c := NewClientWithURL(2*time.Second, "test-token", srv.URL)
	f := NewFetcher(c, "test-token", time.Hour)

	s1 := f.Statuses()
	s2 := f.Statuses()
	if s1 == nil || s2 == nil {
		t.Fatal("expected statuses")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (cache hit)", calls)
	}
}

func TestFetcherErrorCached(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, `{"error":"boom"}`, 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(sampleBody))
	}))
	t.Cleanup(srv.Close)

	c := NewClientWithURL(2*time.Second, "test-token", srv.URL)
	f := NewFetcher(c, "test-token", time.Hour)

	s := f.Statuses()
	if s == nil || s.Error == "" {
		t.Fatalf("expected error status, got %+v", s)
	}
	// La seconde lecture doit servir le cache (même erreur, pas de re-fetch).
	f.Statuses()
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (erreur mise en cache)", calls)
	}
}