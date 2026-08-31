package devin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testServer simule les deux endpoints Devin que le client appelle dans
// l'ordre : /v3/self (identité + org_id) puis /v3/organizations/{org}/consumption/daily.
func testServer(t *testing.T, selfBody, consumptionBody string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			http.Error(w, "bad auth: "+got, 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/self":
			w.WriteHeader(status)
			w.Write([]byte(selfBody))
		case strings.Contains(r.URL.Path, "/consumption/daily"):
			w.WriteHeader(status)
			w.Write([]byte(consumptionBody))
		default:
			http.Error(w, "unknown path "+r.URL.Path, 404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const selfBody = `{
  "principal_type": "pat_user",
  "user_id": "user-05c1133386c44f489b380f0ae8b26f00",
  "user_name": "René",
  "org_id": "org-93932dfb42b443c78ba280183a3d697d"
}`

const consumptionBody = `{
  "total_acus": 42.5,
  "consumption_by_date": [
    {"date": 1788393600, "acus": 12.5,
     "acus_by_product": {"devin": 12.5, "cascade": 0, "terminal": 0, "review": null}},
    {"date": 1788480000, "acus": 30.0,
     "acus_by_product": {"devin": 20.0, "cascade": 5.0, "terminal": 5.0, "review": 0}}
  ]
}`

func TestParseResponse(t *testing.T) {
	s, err := parseResponse([]byte(consumptionBody), "org-93932dfb42b443c78ba280183a3d697d")
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if s.ACUConsumed != 42.5 {
		t.Errorf("ACUConsumed = %v, want 42.5", s.ACUConsumed)
	}
	if s.DayCount != 2 {
		t.Errorf("DayCount = %d, want 2", s.DayCount)
	}
	if s.OrgID != "org-93932dfb42b443c78ba280183a3d697d" {
		t.Errorf("OrgID = %q", s.OrgID)
	}
	if len(s.Days) != 2 {
		t.Fatalf("Days len = %d, want 2", len(s.Days))
	}
	if s.Days[0].ACUs != 12.5 || s.Days[0].ACUsByProduct.Devin != 12.5 {
		t.Errorf("day0 = %+v", s.Days[0])
	}
	if s.Error != "" {
		t.Errorf("unexpected error: %s", s.Error)
	}
}

func TestParseResponseEmpty(t *testing.T) {
	s, err := parseResponse([]byte(`{"total_acus":0.0,"consumption_by_date":[]}`), "org-x")
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if s.ACUConsumed != 0 || s.DayCount != 0 {
		t.Errorf("expected empty consumption, got %+v", s)
	}
}

func TestParseResponseMalformed(t *testing.T) {
	_, err := parseResponse([]byte(`{not json`), "org-x")
	if err == nil {
		t.Fatal("expected JSON parse error")
	}
}

func TestFetchStatusOK(t *testing.T) {
	srv := testServer(t, selfBody, consumptionBody, 200)
	c := NewClientWithURL(2*time.Second, "test-token", srv.URL)
	s, err := c.FetchStatus()
	if err != nil {
		t.Fatalf("FetchStatus: %v", err)
	}
	if s.ACUConsumed != 42.5 {
		t.Errorf("ACUConsumed = %v, want 42.5", s.ACUConsumed)
	}
	if s.OrgID != "org-93932dfb42b443c78ba280183a3d697d" {
		t.Errorf("OrgID = %q", s.OrgID)
	}
	if s.FetchedAt.IsZero() {
		t.Error("FetchedAt should be set")
	}
}

func TestFetchStatusBadToken(t *testing.T) {
	srv := testServer(t, `{"error":"unauthorized"}`, `{}`, 401)
	c := NewClientWithURL(2*time.Second, "test-token", srv.URL)
	_, err := c.FetchStatus()
	if err == nil {
		t.Fatal("expected auth error")
	}
	if !strings.Contains(err.Error(), "invalide ou expiré") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFetchStatusNoOrg(t *testing.T) {
	srv := testServer(t, `{"principal_type":"pat_user"}`, `{}`, 200)
	c := NewClientWithURL(2*time.Second, "test-token", srv.URL)
	_, err := c.FetchStatus()
	if err == nil {
		t.Fatal("expected missing org_id error")
	}
	if !strings.Contains(err.Error(), "org_id") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFetchStatusServerError(t *testing.T) {
	srv := testServer(t, selfBody, `{"error":"boom"}`, 500)
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
	var fetches int // un fetch = paire self + consumption
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v3/self" {
			fetches++
			w.Write([]byte(selfBody))
			return
		}
		w.Write([]byte(consumptionBody))
	}))
	t.Cleanup(srv.Close)

	c := NewClientWithURL(2*time.Second, "test-token", srv.URL)
	f := NewFetcher(c, "test-token", time.Hour)

	s1 := f.Statuses()
	s2 := f.Statuses()
	if s1 == nil || s2 == nil {
		t.Fatal("expected statuses")
	}
	if fetches != 1 {
		t.Errorf("self fetches = %d, want 1 (cache hit)", fetches)
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
		if r.URL.Path == "/v3/self" {
			w.Write([]byte(selfBody))
			return
		}
		w.Write([]byte(consumptionBody))
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