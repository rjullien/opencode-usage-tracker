package devin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// devinServer est un faux serveur Devin qui enregistre ce que le client envoie
// réellement (en-tête Authorization, chemin et query de consumption/daily) :
// c'est la seule preuve disponible ici, aucun token Devin n'existe dans
// l'environnement de test pour appeler api.devin.ai.
type devinServer struct {
	URL string

	mu               sync.Mutex
	authSeen         []string
	consumptionPath  string
	consumptionQuery url.Values
}

func (s *devinServer) auth(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.authSeen) {
		return ""
	}
	return s.authSeen[i]
}

func (s *devinServer) consumption() (string, url.Values) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.consumptionPath, s.consumptionQuery
}

// testServer simule les deux endpoints Devin que le client appelle dans
// l'ordre : /v3/self (identité, org_id éventuel) puis
// /v3/organizations/{org}/consumption/daily. L'en-tête Authorization doit être
// exactement « Bearer test-token » : tout espace ou \n parasite est un échec.
func testServer(t *testing.T, selfBody, consumptionBody string, status int) *devinServer {
	t.Helper()
	rec := &devinServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		rec.mu.Lock()
		rec.authSeen = append(rec.authSeen, got)
		rec.mu.Unlock()

		if got != "Bearer test-token" {
			http.Error(w, "bad auth: "+got, 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/self":
			w.WriteHeader(status)
			w.Write([]byte(selfBody))
		case strings.Contains(r.URL.Path, "/consumption/daily"):
			rec.mu.Lock()
			rec.consumptionPath = r.URL.Path
			rec.consumptionQuery = r.URL.Query()
			rec.mu.Unlock()
			w.WriteHeader(status)
			w.Write([]byte(consumptionBody))
		default:
			http.Error(w, "unknown path "+r.URL.Path, 404)
		}
	}))
	t.Cleanup(srv.Close)
	rec.URL = srv.URL
	return rec
}

// testConfig est la configuration commune des tests : token attendu par
// testServer, timeout court, URL du faux serveur.
func testConfig(baseURL string) Config {
	return Config{Token: "test-token", Timeout: 2 * time.Second, BaseURL: baseURL}
}

// frozen fige l'horloge du client pour rendre les bornes de cycle assertables.
func frozen(c *Client, at time.Time) *Client {
	c.now = func() time.Time { return at }
	return c
}

const selfBody = `{
  "principal_type": "pat_user",
  "user_id": "user-05c1133386c44f489b380f0ae8b26f00",
  "user_name": "René",
  "api_key_id": "key-1",
  "api_key_name": "board",
  "org_id": "org-93932dfb42b443c78ba280183a3d697d"
}`

// selfBodySansOrg est le cas qui cassait la section Devin en production : un PAT
// valide dont /v3/self ne porte AUCUN org_id (nullable et non requis en spec v3).
const selfBodySansOrg = `{
  "principal_type": "pat_user",
  "user_id": "user-x",
  "user_name": "René",
  "api_key_id": "k",
  "api_key_name": "n"
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

// Repères de cycle utilisés par les tests, en dur pour ne pas re-dériver les
// bornes avec la fonction testée : 05/09/2026 08:00 UTC et 05/10/2026 08:00 UTC
// (frontière minuit PST documentée par la spec de consumption/daily).
const (
	cycleStartSept2026 = int64(1788595200) // 2026-09-05T08:00:00Z
	cycleStartOct2026  = int64(1791187200) // 2026-10-05T08:00:00Z
	cycleStartAug2026  = int64(1785916800) // 2026-08-05T08:00:00Z
)

func TestParseResponse(t *testing.T) {
	start := time.Unix(cycleStartSept2026, 0).UTC()
	end := time.Unix(cycleStartOct2026, 0).UTC()
	s, err := parseResponse([]byte(consumptionBody), "org-93932dfb42b443c78ba280183a3d697d", start, end)
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
	if !s.CycleStart.Equal(start) || !s.CycleEnd.Equal(end) {
		t.Errorf("cycle = %v → %v, want %v → %v", s.CycleStart, s.CycleEnd, start, end)
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
	s, err := parseResponse([]byte(`{"total_acus":0.0,"consumption_by_date":[]}`), "org-x", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if s.ACUConsumed != 0 || s.DayCount != 0 {
		t.Errorf("expected empty consumption, got %+v", s)
	}
}

func TestParseResponseMalformed(t *testing.T) {
	_, err := parseResponse([]byte(`{not json`), "org-x", time.Time{}, time.Time{})
	if err == nil {
		t.Fatal("expected JSON parse error")
	}
}

func TestFetchStatusOK(t *testing.T) {
	srv := testServer(t, selfBody, consumptionBody, 200)
	c := NewClient(testConfig(srv.URL))
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

func TestFetchStatusServerError(t *testing.T) {
	srv := testServer(t, selfBody, `{"error":"boom"}`, 500)
	c := NewClient(testConfig(srv.URL))
	_, err := c.FetchStatus()
	if err == nil {
		t.Fatal("expected server error")
	}
}

// ---- Cause 1 : org_id nullable pour un PAT ----

func TestFetchStatusOrgIDDeConfigQuandSelfSansOrg(t *testing.T) {
	srv := testServer(t, selfBodySansOrg, consumptionBody, 200)
	cfg := testConfig(srv.URL)
	cfg.OrgID = "org-de-la-config"
	c := NewClient(cfg)

	s, err := c.FetchStatus()
	if err != nil {
		t.Fatalf("FetchStatus: %v (un PAT sans org doit fonctionner avec DEVIN_ORG_ID)", err)
	}
	path, _ := srv.consumption()
	if want := "/v3/organizations/org-de-la-config/consumption/daily"; path != want {
		t.Errorf("chemin consumption = %q, want %q", path, want)
	}
	if s.OrgID != "org-de-la-config" {
		t.Errorf("Status.OrgID = %q, want org-de-la-config", s.OrgID)
	}
	if s.ACUConsumed != 42.5 || s.DayCount != 2 {
		t.Errorf("statut incomplet : %+v", s)
	}
}

func TestFetchStatusOrgIDDeConfigPrimeSurSelf(t *testing.T) {
	srv := testServer(t, selfBody, consumptionBody, 200)
	cfg := testConfig(srv.URL)
	cfg.OrgID = "org-explicite"
	c := NewClient(cfg)

	if _, err := c.FetchStatus(); err != nil {
		t.Fatalf("FetchStatus: %v", err)
	}
	path, _ := srv.consumption()
	if !strings.Contains(path, "org-explicite") {
		t.Errorf("chemin consumption = %q, DEVIN_ORG_ID doit primer sur /v3/self", path)
	}
}

func TestFetchStatusSansOrgNiConfig(t *testing.T) {
	srv := testServer(t, selfBodySansOrg, `{}`, 200)
	c := NewClient(testConfig(srv.URL))

	_, err := c.FetchStatus()
	if err == nil {
		t.Fatal("expected missing org error")
	}
	for _, want := range []string{"org_id", "DEVIN_ORG_ID", "pat_user"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message d'erreur sans %q : %v", want, err)
		}
	}
}

// ---- Cause 2 : token non trimmé et diagnostic ----

func TestTokenTrimmeAvantEnvoi(t *testing.T) {
	srv := testServer(t, selfBody, consumptionBody, 200)
	cfg := testConfig(srv.URL)
	cfg.Token = "test-token\n" // exactement ce qu'un secret Kubernetes fournit
	c := NewClient(cfg)

	if _, err := c.FetchStatus(); err != nil {
		t.Fatalf("FetchStatus: %v (le \\n final doit être retiré avant l'en-tête)", err)
	}
	if got := srv.auth(0); got != "Bearer test-token" {
		t.Errorf("en-tête Authorization = %q, want %q", got, "Bearer test-token")
	}
}

func TestTokenFromEnvTrimEspaces(t *testing.T) {
	t.Setenv("DEVIN_API_KEY", " cog_abc \n")
	if got := TokenFromEnv(); got != "cog_abc" {
		t.Errorf("TokenFromEnv() = %q, want %q", got, "cog_abc")
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("DEVIN_API_KEY", "cog_abc\n")
	t.Setenv("DEVIN_ORG_ID", " org-42 \n")
	t.Setenv("DEVIN_RESET_DAY", "12")

	cfg := ConfigFromEnv(3 * time.Second)
	if cfg.Token != "cog_abc" || cfg.OrgID != "org-42" || cfg.ResetDay != 12 {
		t.Errorf("ConfigFromEnv = %+v", cfg)
	}
	if cfg.Timeout != 3*time.Second {
		t.Errorf("Timeout = %v, want 3s", cfg.Timeout)
	}
}

func TestFingerprint(t *testing.T) {
	const token = "cog_0123456789abcdef"
	got := Fingerprint(token)

	if strings.Contains(got, token) {
		t.Fatalf("l'empreinte expose le token : %q", got)
	}
	if !strings.HasPrefix(got, "len=20 sha256=") {
		t.Fatalf("Fingerprint = %q, want prefix %q", got, "len=20 sha256=")
	}
	hexPart := strings.TrimPrefix(got, "len=20 sha256=")
	if len(hexPart) != 8 {
		t.Errorf("empreinte = %q, want 8 caractères hexadécimaux", hexPart)
	}
	for _, r := range hexPart {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Errorf("caractère non hexadécimal %q dans %q", r, hexPart)
		}
	}
	if again := Fingerprint(token); again != got {
		t.Errorf("Fingerprint non déterministe : %q puis %q", got, again)
	}
	if other := Fingerprint("cog_autre-token-different"); other == got {
		t.Error("deux tokens différents donnent la même empreinte")
	}
	// Un token trimmé et le même avec un \n désignent la clé réellement envoyée.
	if Fingerprint(token+"\n") != got {
		t.Error("l'empreinte doit décrire le token trimmé, celui réellement envoyé")
	}
	if got := Fingerprint(""); !strings.Contains(got, "len=0") {
		t.Errorf("Fingerprint(\"\") = %q, want une mention explicite de l'absence", got)
	}
}

func TestFetchStatus401MessageDiagnostique(t *testing.T) {
	srv := testServer(t, `{"error":"unauthorized"}`, `{}`, 401)
	cfg := testConfig(srv.URL)
	c := NewClient(cfg)

	_, err := c.FetchStatus()
	if err == nil {
		t.Fatal("expected auth error")
	}
	msg := err.Error()
	for _, want := range []string{"HTTP 401", "DEVIN_API_KEY", Fingerprint("test-token")} {
		if !strings.Contains(msg, want) {
			t.Errorf("message sans %q : %s", want, msg)
		}
	}
	if strings.Contains(msg, "test-token") {
		t.Errorf("le token brut apparaît dans le message : %s", msg)
	}
}

// ---- Cause 3 : fenêtrage sur le cycle de facturation ----

func TestCycleBounds(t *testing.T) {
	cases := []struct {
		name       string
		now        string
		resetDay   int
		wantStart  int64
		wantEndUTC string
	}{
		{
			name:       "avant le jour de reset : cycle commencé le mois précédent",
			now:        "2026-09-03T10:00:00Z",
			resetDay:   5,
			wantStart:  cycleStartAug2026,
			wantEndUTC: "2026-09-05T08:00:00Z",
		},
		{
			name:       "le jour de reset à 07:00 UTC : frontière 08:00 pas atteinte",
			now:        "2026-09-05T07:00:00Z",
			resetDay:   5,
			wantStart:  cycleStartAug2026,
			wantEndUTC: "2026-09-05T08:00:00Z",
		},
		{
			name:       "le jour de reset à 09:00 UTC : nouveau cycle",
			now:        "2026-09-05T09:00:00Z",
			resetDay:   5,
			wantStart:  cycleStartSept2026,
			wantEndUTC: "2026-10-05T08:00:00Z",
		},
		{
			name:       "après le jour de reset",
			now:        "2026-09-20T12:00:00Z",
			resetDay:   5,
			wantStart:  cycleStartSept2026,
			wantEndUTC: "2026-10-05T08:00:00Z",
		},
		{
			name:       "passage d'année",
			now:        "2027-01-02T00:00:00Z",
			resetDay:   5,
			wantStart:  mustEpoch(t, "2026-12-05T08:00:00Z"),
			wantEndUTC: "2027-01-05T08:00:00Z",
		},
		{
			name:       "resetDay 28 (borne haute admise)",
			now:        "2026-03-01T00:00:00Z",
			resetDay:   28,
			wantStart:  mustEpoch(t, "2026-02-28T08:00:00Z"),
			wantEndUTC: "2026-03-28T08:00:00Z",
		},
		{
			name:       "resetDay 31 hors bornes : clampé au défaut 5",
			now:        "2026-09-20T12:00:00Z",
			resetDay:   31,
			wantStart:  cycleStartSept2026,
			wantEndUTC: "2026-10-05T08:00:00Z",
		},
		{
			name:       "resetDay 0 hors bornes : clampé au défaut 5",
			now:        "2026-09-20T12:00:00Z",
			resetDay:   0,
			wantStart:  cycleStartSept2026,
			wantEndUTC: "2026-10-05T08:00:00Z",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := mustParse(t, tc.now)
			start, end := CycleBounds(now, tc.resetDay)

			if start.Unix() != tc.wantStart {
				t.Errorf("start = %s (%d), want %d", start.UTC().Format(time.RFC3339), start.Unix(), tc.wantStart)
			}
			if got := end.UTC().Format(time.RFC3339); got != tc.wantEndUTC {
				t.Errorf("end = %s, want %s", got, tc.wantEndUTC)
			}
			if !start.Before(end) {
				t.Errorf("start %s doit précéder end %s", start, end)
			}
			if h := start.UTC().Hour(); h != 8 {
				t.Errorf("heure de la borne start = %dh UTC, want 8h (minuit PST)", h)
			}
			if h := end.UTC().Hour(); h != 8 {
				t.Errorf("heure de la borne end = %dh UTC, want 8h (minuit PST)", h)
			}
			if !now.Before(end) || now.Before(start) {
				t.Errorf("now %s hors du cycle %s → %s", now, start, end)
			}
		})
	}
}

func TestFetchStatusEnvoieLaFenetreDuCycle(t *testing.T) {
	srv := testServer(t, selfBody, consumptionBody, 200)
	cfg := testConfig(srv.URL)
	cfg.ResetDay = 5
	// Le 20/09/2026 : le cycle courant va du 05/09 08:00 UTC au 05/10 08:00 UTC.
	c := frozen(NewClient(cfg), mustParse(t, "2026-09-20T12:00:00Z"))

	s, err := c.FetchStatus()
	if err != nil {
		t.Fatalf("FetchStatus: %v", err)
	}

	_, query := srv.consumption()
	if got, want := query.Get("time_after"), "1788595200"; got != want {
		t.Errorf("time_after = %q, want %q (05/09/2026 08:00 UTC)", got, want)
	}
	if got, want := query.Get("time_before"), "1791187200"; got != want {
		t.Errorf("time_before = %q, want %q (05/10/2026 08:00 UTC)", got, want)
	}
	if s.CycleStart.Unix() != 1788595200 || s.CycleEnd.Unix() != 1791187200 {
		t.Errorf("bornes du Status = %d → %d, want 1788595200 → 1791187200",
			s.CycleStart.Unix(), s.CycleEnd.Unix())
	}
}

// ---- Fetcher : contrat non bloquant ----

func TestFetcherNoToken(t *testing.T) {
	f := NewFetcher(nil, "", time.Minute)
	if s := f.Statuses(); s != nil {
		t.Fatalf("expected nil status without token, got %+v", s)
	}
}

func TestFetcherTokenBlancIgnore(t *testing.T) {
	// Un secret vide monté depuis Kubernetes vaut souvent "\n" : ce n'est pas
	// un token, la section doit rester absente au lieu de boucler sur des 401.
	f := NewFetcher(nil, " \n", time.Minute)
	if s := f.Statuses(); s != nil {
		t.Fatalf("expected nil status for blank token, got %+v", s)
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

	c := NewClient(testConfig(srv.URL))
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

	c := NewClient(testConfig(srv.URL))
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

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("date de test invalide %q: %v", s, err)
	}
	return ts
}

func mustEpoch(t *testing.T, s string) int64 {
	t.Helper()
	return mustParse(t, s).Unix()
}
