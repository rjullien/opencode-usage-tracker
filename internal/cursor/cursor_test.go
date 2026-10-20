package cursor

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// makeJWT builds an unsigned JWT with the given claims (tests only).
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func boolPtr(v bool) *bool { return &v }

// cursorServer mocks oauth + DashboardService + optional web Grok fallback.
type cursorServer struct {
	URL string

	mu sync.Mutex

	oauthCalls   int
	periodCalls  int
	planCalls    int
	sandCalls    int
	sandWebCalls int

	oauthStatus int
	oauthBody   string

	periodStatus int
	periodBody   string
	planStatus   int
	planBody     string
	sandStatus   int
	sandBody     string
	sandWebStatus int
	sandWebBody   string

	lastAuth   string
	lastCookie string
	seenRefresh string
}

func newCursorServer(t *testing.T) *cursorServer {
	t.Helper()
	s := &cursorServer{
		oauthStatus:  200,
		periodStatus: 200,
		planStatus:   200,
		sandStatus:   200,
		sandWebStatus: 200,
		oauthBody:    "", // filled per-test
		periodBody: `{
			"billingCycleStart":"2026-09-01T00:00:00Z",
			"billingCycleEnd":"2026-10-01T00:00:00Z",
			"planUsage":{
				"totalPercentUsed":42.5,
				"autoPercentUsed":30,
				"apiPercentUsed":12.5,
				"totalSpend":4250,
				"limit":10000
			}
		}`,
		planBody: `{"planInfo":{"planName":"Pro"}}`,
		sandBody: `{
			"usagePercent":18.5,
			"hasNonZeroIncludedLimit":true,
			"nextResetTimestampUtc":"2026-09-19T00:00:00Z"
		}`,
		sandWebBody: `{"usagePercent":99,"hasNonZeroIncludedLimit":true}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		switch {
		case r.URL.Path == "/oauth/token" || strings.HasSuffix(r.URL.Path, "/oauth/token"):
			s.oauthCalls++
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.seenRefresh = body["refresh_token"]
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s.oauthStatus)
			if s.oauthBody != "" {
				w.Write([]byte(s.oauthBody))
			} else {
				w.Write([]byte(`{"access_token":"missing"}`))
			}
			return

		case strings.Contains(r.URL.Path, "GetCurrentPeriodUsage"):
			s.periodCalls++
			s.lastAuth = r.Header.Get("Authorization")
			if r.Header.Get("Connect-Protocol-Version") != "1" {
				http.Error(w, "missing Connect-Protocol-Version", 400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s.periodStatus)
			w.Write([]byte(s.periodBody))
			return

		case strings.Contains(r.URL.Path, "GetPlanInfo"):
			s.planCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s.planStatus)
			w.Write([]byte(s.planBody))
			return

		case strings.Contains(r.URL.Path, "GetSandUsageStatus"):
			s.sandCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s.sandStatus)
			w.Write([]byte(s.sandBody))
			return

		case strings.Contains(r.URL.Path, "get-sand-usage-status"):
			s.sandWebCalls++
			s.lastCookie = r.Header.Get("Cookie")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s.sandWebStatus)
			w.Write([]byte(s.sandWebBody))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

func (s *cursorServer) client(t *testing.T, refresh string, accessJWT string) *Client {
	t.Helper()
	if accessJWT == "" {
		accessJWT = makeJWT(t, map[string]any{
			"sub": "auth0|user_test",
			"exp": time.Now().Add(time.Hour).Unix(),
		})
	}
	s.mu.Lock()
	logout := false
	s.oauthBody = `{"access_token":"` + accessJWT + `","id_token":"id","shouldLogout":false}`
	_ = logout
	s.mu.Unlock()

	return NewClient(Config{
		RefreshToken: refresh,
		Timeout:      2 * time.Second,
		APIBase:      s.URL,
		OAuthURL:     s.URL + "/oauth/token",
		WebBase:      s.URL,
	})
}

func TestFetchStatusOK(t *testing.T) {
	srv := newCursorServer(t)
	c := srv.client(t, "refresh-secret", "")

	st, err := c.FetchStatus()
	if err != nil {
		t.Fatalf("FetchStatus: %v", err)
	}
	if st.PlanName != "Pro" {
		t.Errorf("PlanName = %q, want Pro", st.PlanName)
	}
	if st.Period == nil {
		t.Fatal("Period nil")
	}
	if st.Period.TotalPercent != 42.5 || st.Period.AutoPercent != 30 || st.Period.APIPercent != 12.5 {
		t.Errorf("percents = %+v", st.Period)
	}
	if st.Period.SpendCents != 4250 || st.Period.LimitCents != 10000 {
		t.Errorf("spend/limit = %d/%d", st.Period.SpendCents, st.Period.LimitCents)
	}
	if st.Period.CycleEnd == nil || st.Period.CycleEnd.UTC().Format("2006-01-02") != "2026-10-01" {
		t.Errorf("CycleEnd = %v", st.Period.CycleEnd)
	}
	if st.Grok == nil || st.Grok.UsagePercent != 18.5 {
		t.Errorf("Grok = %+v", st.Grok)
	}
	if st.Grok.ResetsAt == nil {
		t.Error("Grok.ResetsAt nil")
	}
	if srv.oauthCalls < 1 || srv.periodCalls != 1 {
		t.Errorf("calls oauth=%d period=%d", srv.oauthCalls, srv.periodCalls)
	}
	if !strings.HasPrefix(srv.lastAuth, "Bearer ") {
		t.Errorf("Authorization = %q", srv.lastAuth)
	}
}

func TestGrokOmittedWithoutAllowance(t *testing.T) {
	srv := newCursorServer(t)
	srv.sandBody = `{"usagePercent":0,"hasNonZeroIncludedLimit":false}`
	c := srv.client(t, "refresh-secret", "")

	st, err := c.FetchStatus()
	if err != nil {
		t.Fatalf("FetchStatus: %v", err)
	}
	if st.Period == nil {
		t.Fatal("Period should still be present")
	}
	if st.Grok != nil {
		t.Errorf("Grok = %+v, want nil (no allowance)", st.Grok)
	}
	if st.GrokError != "" {
		t.Errorf("GrokError = %q, want empty (omission is not an error)", st.GrokError)
	}
}

func TestGrokSoftFailKeepsPeriod(t *testing.T) {
	srv := newCursorServer(t)
	srv.sandStatus = 500
	srv.sandBody = `boom`
	srv.sandWebStatus = 500
	srv.sandWebBody = `boom`
	c := srv.client(t, "refresh-secret", "")

	st, err := c.FetchStatus()
	if err != nil {
		t.Fatalf("FetchStatus: %v (Grok soft-fail must not fail the whole fetch)", err)
	}
	if st.Period == nil || st.Period.TotalPercent != 42.5 {
		t.Errorf("Period = %+v", st.Period)
	}
	if st.Grok != nil {
		t.Errorf("Grok = %+v, want nil", st.Grok)
	}
	if st.GrokError == "" {
		t.Error("GrokError empty, want diagnostic")
	}
}

func TestGrokWebFallback(t *testing.T) {
	srv := newCursorServer(t)
	srv.sandStatus = 404
	srv.sandBody = `not found`
	access := makeJWT(t, map[string]any{
		"sub": "auth0|user_abc",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	c := srv.client(t, "refresh-secret", access)

	st, err := c.FetchStatus()
	if err != nil {
		t.Fatalf("FetchStatus: %v", err)
	}
	if st.Grok == nil || st.Grok.UsagePercent != 99 {
		t.Errorf("Grok = %+v (web fallback)", st.Grok)
	}
	if srv.sandWebCalls < 1 {
		t.Error("expected web fallback call")
	}
	if !strings.Contains(srv.lastCookie, "WorkosCursorSessionToken=user_abc%3A%3A") {
		t.Errorf("Cookie = %q", srv.lastCookie)
	}
}

func TestShouldLogoutIsAuthError(t *testing.T) {
	srv := newCursorServer(t)
	srv.oauthStatus = 200
	srv.oauthBody = `{"access_token":"","shouldLogout":true}`
	c := NewClient(Config{
		RefreshToken: "revoked",
		Timeout:      time.Second,
		APIBase:      srv.URL,
		OAuthURL:     srv.URL + "/oauth/token",
		WebBase:      srv.URL,
	})

	_, err := c.FetchStatus()
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsAuthError(err) {
		t.Errorf("err = %v (%T), want AuthError", err, err)
	}
	if !strings.Contains(err.Error(), "shouldLogout") {
		t.Errorf("err = %q, want shouldLogout mention", err)
	}
}

func TestRefreshOn401(t *testing.T) {
	srv := newCursorServer(t)
	var periodHits atomic.Int32
	// Custom handler: first period call 401, second 200 after re-oauth.
	mux := http.NewServeMux()
	access1 := makeJWT(t, map[string]any{"sub": "auth0|u", "exp": time.Now().Add(time.Hour).Unix()})
	access2 := makeJWT(t, map[string]any{"sub": "auth0|u", "exp": time.Now().Add(2 * time.Hour).Unix()})
	var oauthN atomic.Int32
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		n := oauthN.Add(1)
		tok := access1
		if n > 1 {
			tok = access2
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"` + tok + `","shouldLogout":false}`))
	})
	mux.HandleFunc("/aiserver.v1.DashboardService/GetCurrentPeriodUsage", func(w http.ResponseWriter, r *http.Request) {
		n := periodHits.Add(1)
		if n == 1 {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(srv.periodBody))
	})
	mux.HandleFunc("/aiserver.v1.DashboardService/GetPlanInfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"planInfo":{"planName":"Pro"}}`))
	})
	mux.HandleFunc("/aiserver.v1.DashboardService/GetSandUsageStatus", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"usagePercent":1,"hasNonZeroIncludedLimit":true}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	c := NewClient(Config{
		RefreshToken: "refresh-secret",
		Timeout:      2 * time.Second,
		APIBase:      ts.URL,
		OAuthURL:     ts.URL + "/oauth/token",
		WebBase:      ts.URL,
	})
	st, err := c.FetchStatus()
	if err != nil {
		t.Fatalf("FetchStatus: %v", err)
	}
	if st.Period == nil {
		t.Fatal("Period nil after 401 retry")
	}
	if oauthN.Load() < 2 {
		t.Errorf("oauth calls = %d, want >= 2 (initial + after 401)", oauthN.Load())
	}
	if periodHits.Load() < 2 {
		t.Errorf("period calls = %d, want >= 2", periodHits.Load())
	}
}

func TestRotatedRefreshTokenPreferredInMemory(t *testing.T) {
	srv := newCursorServer(t)
	access := makeJWT(t, map[string]any{"sub": "auth0|u", "exp": time.Now().Add(time.Hour).Unix()})
	var gotRotated string
	srv.mu.Lock()
	srv.oauthBody = `{"access_token":"` + access + `","refresh_token":"new-refresh","shouldLogout":false}`
	srv.mu.Unlock()

	c := NewClient(Config{
		RefreshToken: "old-refresh",
		Timeout:      time.Second,
		APIBase:      srv.URL,
		OAuthURL:     srv.URL + "/oauth/token",
		WebBase:      srv.URL,
	})
	c.auth.onRotated = func(r string) { gotRotated = r }

	if _, err := c.FetchStatus(); err != nil {
		t.Fatalf("FetchStatus: %v", err)
	}
	if gotRotated != "new-refresh" {
		t.Errorf("onRotated = %q, want new-refresh", gotRotated)
	}
	if c.auth.bundle.refresh != "new-refresh" {
		t.Errorf("in-memory refresh = %q, want new-refresh", c.auth.bundle.refresh)
	}
}

func TestFetcherNilWithoutToken(t *testing.T) {
	f := NewFetcher(nil, "", time.Minute)
	if f.Statuses() != nil {
		t.Fatal("expected nil without token")
	}
}

func TestFetcherCachesAndSurfacesAuthError(t *testing.T) {
	srv := newCursorServer(t)
	c := srv.client(t, "refresh-secret", "")
	f := NewFetcher(c, "refresh-secret", time.Minute)

	st := f.Statuses()
	if st == nil || st.Period == nil {
		t.Fatalf("Statuses = %+v", st)
	}
	firstOAuth := srv.oauthCalls
	_ = f.Statuses()
	if srv.oauthCalls != firstOAuth || srv.periodCalls != 1 {
		t.Errorf("cache miss: oauth=%d period=%d", srv.oauthCalls, srv.periodCalls)
	}

	// Auth failure path: empty token on a fresh fetcher still returns a Status with Error.
	srv.oauthStatus = 401
	srv.oauthBody = `{"error":"invalid_grant"}`
	f2 := NewFetcher(NewClient(Config{
		RefreshToken: "bad",
		Timeout:      time.Second,
		APIBase:      srv.URL,
		OAuthURL:     srv.URL + "/oauth/token",
	}), "bad", time.Minute)
	st2 := f2.Statuses()
	if st2 == nil || st2.Error == "" {
		t.Fatalf("expected error status, got %+v", st2)
	}
	if !strings.Contains(st2.Error, "CURSOR_REFRESH_TOKEN") && !strings.Contains(st2.Error, "refusé") {
		t.Errorf("Error = %q", st2.Error)
	}
}

func TestFingerprintNoLeak(t *testing.T) {
	fp := Fingerprint("super-secret-refresh-token-value")
	if strings.Contains(fp, "super-secret") {
		t.Errorf("Fingerprint leaked token: %s", fp)
	}
	if !strings.Contains(fp, "sha256=") || !strings.Contains(fp, "len=") {
		t.Errorf("Fingerprint = %q", fp)
	}
}

func TestParseGrokEpochReset(t *testing.T) {
	raw := []byte(`{"usagePercent":10,"hasNonZeroIncludedLimit":true,"nextResetTimestampUtc":1789776000}`)
	g, err := parseGrokUsage(raw)
	if err != nil || g == nil || g.ResetsAt == nil {
		t.Fatalf("parseGrokUsage = %+v err=%v", g, err)
	}
	if g.ResetsAt.UTC().Format("2006-01-02") != "2026-09-19" {
		t.Errorf("ResetsAt = %s", g.ResetsAt.UTC())
	}
}

func TestJwtSubStripsProviderPrefix(t *testing.T) {
	tok := makeJWT(t, map[string]any{"sub": "google-oauth2|user_xyz", "exp": 9999999999})
	sub, ok := jwtSub(tok)
	if !ok || sub != "user_xyz" {
		t.Errorf("jwtSub = %q ok=%v", sub, ok)
	}
}
