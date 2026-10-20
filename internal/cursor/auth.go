package cursor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Auth Cursor — deux modes, adaptés à Infisical.
//
// 1) Clé API utilisateur (canonique) : CURSOR_API_KEY (préfixe crsr_…).
//    POST https://api2.cursor.sh/auth/exchange_user_api_key
//    Authorization: Bearer <crsr_…>, body {}
//    → {accessToken, refreshToken}. L'access JWT (~1 h) sert de Bearer aux
//    appels DashboardService. Le refreshToken renvoyé ne se renouvelle PAS via
//    /oauth/token : on re-échange la clé à l'expiration ou sur 401.
//
// 2) Refresh OAuth (historique) : CURSOR_REFRESH_TOKEN (SQLite / `agent login`).
//    POST https://api2.cursor.sh/oauth/token
//    {"grant_type":"refresh_token","client_id":"…","refresh_token":"…"}
//
// Filet de sécurité : si CURSOR_REFRESH_TOKEN commence par « crsr_ », on le
// traite comme une clé API (échange), pour un déploiement qui aurait écrasé
// le secret refresh sans renommer la variable.
//
// Priorité : CURSOR_API_KEY > CURSOR_REFRESH_TOKEN.
// Access JWT : mémoire process uniquement. Jamais logué en clair.
//
// ⚠️ Endpoints non documentés / non officiels — susceptibles de changer.

const (
	defaultOAuthURL    = "https://api2.cursor.sh/oauth/token"
	defaultExchangeURL = "https://api2.cursor.sh/auth/exchange_user_api_key"
	// oauthClientID est le client_id public de l'app Cursor (OpenUsage / OpenQuota / AI-Usage).
	oauthClientID = "KbZUR41cY7W6zRSdpSUJ7I7mLYBKOCmB"
	// refreshSkew : on renouvelle l'access JWT s'il expire dans moins de 3 min
	// (fenêtre 2–5 min demandée), ou immédiatement sur 401.
	refreshSkew = 3 * time.Minute

	apiKeyPrefix = "crsr_"
)

// authMode distingue l'échange clé API du refresh OAuth.
type authMode int

const (
	authModeNone authMode = iota
	authModeAPIKey
	authModeRefresh
)

func (m authMode) String() string {
	switch m {
	case authModeAPIKey:
		return "api_key"
	case authModeRefresh:
		return "oauth_refresh"
	default:
		return "none"
	}
}

// AuthError signale une clé/refresh révoqué ou shouldLogout — le dashboard doit
// afficher un message d'auth, pas planter.
type AuthError struct {
	Msg string
}

func (e *AuthError) Error() string { return e.Msg }

// IsAuthError rapporte si err est (ou enveloppe) une AuthError.
func IsAuthError(err error) bool {
	_, ok := err.(*AuthError)
	return ok
}

type tokenBundle struct {
	access  string
	refresh string // OAuth only ; ignoré en mode api_key (re-échange de la clé)
	exp     time.Time
}

// authenticator gère l'obtention d'un access JWT (échange clé ou OAuth) en mémoire.
type authenticator struct {
	http        *http.Client
	oauthURL    string
	exchangeURL string
	clientID    string
	mode        authMode
	secretVar   string // nom de la variable réellement utilisée (messages d'erreur)
	apiKey      string // durable en mode api_key ; jamais remplacé par un refresh échangé
	mu          sync.Mutex
	bundle      tokenBundle
	now         func() time.Time
	onRotated   func(newRefresh string)
}

func newAuthenticator(httpClient *http.Client, cfg Config) *authenticator {
	oauthURL := cfg.OAuthURL
	if oauthURL == "" {
		oauthURL = defaultOAuthURL
	}
	exchangeURL := cfg.ExchangeURL
	if exchangeURL == "" {
		exchangeURL = defaultExchangeURL
	}

	mode, secret, secretVar := cfg.resolveAuth()
	a := &authenticator{
		http:        httpClient,
		oauthURL:    oauthURL,
		exchangeURL: exchangeURL,
		clientID:    oauthClientID,
		mode:        mode,
		secretVar:   secretVar,
		now:         time.Now,
		onRotated: func(newRefresh string) {
			log.Printf("Cursor: nouveau refresh_token reçu — mettre à jour Infisical (ex. /agents/cursor) ; %s",
				Fingerprint(newRefresh))
		},
	}
	switch mode {
	case authModeAPIKey:
		a.apiKey = secret
	case authModeRefresh:
		a.bundle.refresh = secret
	}
	return a
}

// accessToken renvoie un access JWT valide, en renouvelant si besoin.
func (a *authenticator) accessToken() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch a.mode {
	case authModeNone:
		return "", &AuthError{Msg: "aucune credential Cursor (CURSOR_API_KEY ou CURSOR_REFRESH_TOKEN)"}
	case authModeAPIKey:
		if a.apiKey == "" {
			return "", &AuthError{Msg: a.secretVar + " absent"}
		}
	case authModeRefresh:
		if a.bundle.refresh == "" {
			return "", &AuthError{Msg: a.secretVar + " absent"}
		}
	}

	if a.bundle.access != "" && a.now().Add(refreshSkew).Before(a.bundle.exp) {
		return a.bundle.access, nil
	}
	return a.renewLocked()
}

// invalidateAccess force un renouvellement au prochain appel (ex. après 401).
func (a *authenticator) invalidateAccess() {
	a.mu.Lock()
	a.bundle.access = ""
	a.bundle.exp = time.Time{}
	a.mu.Unlock()
}

// secretFingerprint résume la credential durable (clé API ou refresh), jamais l'access.
func (a *authenticator) secretFingerprint() string {
	switch a.mode {
	case authModeAPIKey:
		return Fingerprint(a.apiKey)
	default:
		return Fingerprint(a.bundle.refresh)
	}
}

func (a *authenticator) renewLocked() (string, error) {
	switch a.mode {
	case authModeAPIKey:
		return a.exchangeLocked()
	case authModeRefresh:
		return a.refreshLocked()
	default:
		return "", &AuthError{Msg: "aucune credential Cursor (CURSOR_API_KEY ou CURSOR_REFRESH_TOKEN)"}
	}
}

type oauthResponse struct {
	AccessToken       string `json:"access_token"`
	IDToken           string `json:"id_token"`
	RefreshToken      string `json:"refresh_token"`
	ShouldLogout      *bool  `json:"shouldLogout"`
	ShouldLogoutSnake *bool  `json:"should_logout"`
	Error             string `json:"error"`
	ErrorDescription  string `json:"error_description"`
}

type exchangeResponse struct {
	AccessToken       string `json:"accessToken"`
	RefreshToken      string `json:"refreshToken"`
	AccessTokenSnake  string `json:"access_token"`
	RefreshTokenSnake string `json:"refresh_token"`
	ShouldLogout      *bool  `json:"shouldLogout"`
	ShouldLogoutSnake *bool  `json:"should_logout"`
	Error             string `json:"error"`
	ErrorDescription  string `json:"error_description"`
}

func (e exchangeResponse) access() string {
	if v := strings.TrimSpace(e.AccessToken); v != "" {
		return v
	}
	return strings.TrimSpace(e.AccessTokenSnake)
}

func (a *authenticator) exchangeLocked() (string, error) {
	req, err := http.NewRequest(http.MethodPost, a.exchangeURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "opencode-usage-tracker/2.0")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("network error (exchange_user_api_key): %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read error (exchange_user_api_key): %w", err)
	}

	var er exchangeResponse
	if err := json.Unmarshal(raw, &er); err != nil {
		return "", fmt.Errorf("JSON parse error (exchange_user_api_key): %w", err)
	}

	if logoutFlag(er.ShouldLogout, er.ShouldLogoutSnake) {
		log.Printf("Cursor: shouldLogout=true — %s %s", a.secretVar, Fingerprint(a.apiKey))
		return "", &AuthError{
			Msg: fmt.Sprintf(
				"authentification Cursor révoquée (shouldLogout) : vérifier %s "+
					"(clé API utilisateur crsr_… dans Cursor Dashboard > API Keys) et Infisical",
				a.secretVar),
		}
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		log.Printf("Cursor: exchange refusé (HTTP %d) — %s %s", resp.StatusCode, a.secretVar, Fingerprint(a.apiKey))
		return "", &AuthError{
			Msg: fmt.Sprintf(
				"clé API Cursor refusée (HTTP %d) : %s invalide ou révoquée — "+
					"régénérer dans Cursor Dashboard > API Keys et mettre à jour Infisical "+
					"(longueur et empreinte SHA-256 dans les logs du pod)",
				resp.StatusCode, a.secretVar),
		}
	}
	if resp.StatusCode != http.StatusOK {
		detail := er.Error
		if er.ErrorDescription != "" {
			detail = er.Error + ": " + er.ErrorDescription
		}
		if detail == "" {
			detail = truncate(string(raw), 200)
		}
		return "", fmt.Errorf("exchange_user_api_key HTTP %d: %s", resp.StatusCode, detail)
	}

	access := er.access()
	if access == "" {
		return "", &AuthError{
			Msg: fmt.Sprintf("exchange_user_api_key: accessToken absent — vérifier %s", a.secretVar),
		}
	}

	// Le refreshToken renvoyé par l'échange n'est PAS utilisable via /oauth/token :
	// on l'ignore volontairement et on re-échangera la clé durable.
	exp, ok := jwtExp(access)
	if !ok {
		exp = a.now().Add(refreshSkew)
	}

	a.bundle.access = access
	a.bundle.exp = exp
	return access, nil
}

func (a *authenticator) refreshLocked() (string, error) {
	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     a.clientID,
		"refresh_token": a.bundle.refresh,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, a.oauthURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "opencode-usage-tracker/2.0")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("network error (oauth/token): %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read error (oauth/token): %w", err)
	}

	var or oauthResponse
	if err := json.Unmarshal(raw, &or); err != nil {
		return "", fmt.Errorf("JSON parse error (oauth/token): %w", err)
	}

	if logoutFlag(or.ShouldLogout, or.ShouldLogoutSnake) {
		log.Printf("Cursor: shouldLogout=true — %s %s", a.secretVar, Fingerprint(a.bundle.refresh))
		return "", &AuthError{
			Msg: fmt.Sprintf(
				"authentification Cursor révoquée (shouldLogout) : renouveler %s "+
					"(SQLite cursorAuth/refreshToken ou `agent login`) et mettre à jour Infisical",
				a.secretVar),
		}
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		log.Printf("Cursor: oauth refusé (HTTP %d) — %s %s", resp.StatusCode, a.secretVar, Fingerprint(a.bundle.refresh))
		return "", &AuthError{
			Msg: fmt.Sprintf(
				"token Cursor refusé (HTTP %d) : refresh expiré ou révoqué — vérifier %s "+
					"(longueur et empreinte SHA-256 dans les logs du pod)",
				resp.StatusCode, a.secretVar),
		}
	}
	if resp.StatusCode != http.StatusOK {
		detail := or.Error
		if or.ErrorDescription != "" {
			detail = or.Error + ": " + or.ErrorDescription
		}
		if detail == "" {
			detail = truncate(string(raw), 200)
		}
		return "", fmt.Errorf("oauth/token HTTP %d: %s", resp.StatusCode, detail)
	}

	access := strings.TrimSpace(or.AccessToken)
	if access == "" {
		return "", &AuthError{Msg: fmt.Sprintf("oauth/token: access_token absent — renouveler %s", a.secretVar)}
	}

	exp, ok := jwtExp(access)
	if !ok {
		// Sans exp lisible, on accepte le token mais on le traitera comme
		// bientôt expiré au prochain cycle (force refresh fréquent).
		exp = a.now().Add(refreshSkew)
	}

	if rotated := strings.TrimSpace(or.RefreshToken); rotated != "" && rotated != a.bundle.refresh {
		a.bundle.refresh = rotated
		if a.onRotated != nil {
			a.onRotated(rotated)
		}
	}

	a.bundle.access = access
	a.bundle.exp = exp
	return access, nil
}

func logoutFlag(camel, snake *bool) bool {
	if camel != nil && *camel {
		return true
	}
	if snake != nil && *snake {
		return true
	}
	return false
}

// jwtExp lit claim exp sans vérifier la signature (on ne fait que planifier
// le refresh ; la validité est tranchée par l'API Cursor).
func jwtExp(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Time{}, false
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0).UTC(), true
}

// jwtSub extrait le subject du JWT (cookie WorkosCursorSessionToken).
func jwtSub(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return "", false
		}
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", false
	}
	sub := strings.TrimSpace(claims.Sub)
	if sub == "" {
		return "", false
	}
	// Forme WorkOS fréquente : "auth0|user_xxx" → id session = second segment.
	if _, id, ok := strings.Cut(sub, "|"); ok && strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id), true
	}
	return sub, true
}
