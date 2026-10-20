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

// OAuth Cursor — renouvelable, adapté à Infisical.
//
// Secret durable : CURSOR_REFRESH_TOKEN (SQLite Cursor cursorAuth/refreshToken,
// ou keychain CLI `agent login` : cursor-refresh-token).
// Access JWT : mémoire process uniquement, jamais exigé dans Infisical.
//
//	POST https://api2.cursor.sh/oauth/token
//	{"grant_type":"refresh_token","client_id":"…","refresh_token":"…"}
//
// Succès : {access_token, id_token, shouldLogout:false}.
// Révoqué : shouldLogout:true → erreur d'auth claire, sans faire planter le board.
//
// Cursor ne consomme en général pas le refresh (RFC 6749 §6). Si une réponse
// porte un nouveau refresh_token, on le préfère en mémoire et on documente
// qu'Infisical doit être mis à jour.
//
// ⚠️ Endpoints non documentés / non officiels — susceptibles de changer.

const (
	defaultOAuthURL = "https://api2.cursor.sh/oauth/token"
	// oauthClientID est le client_id public de l'app Cursor (OpenUsage / OpenQuota / AI-Usage).
	oauthClientID = "KbZUR41cY7W6zRSdpSUJ7I7mLYBKOCmB"
	// refreshSkew : on renouvelle l'access JWT s'il expire dans moins de 3 min
	// (fenêtre 2–5 min demandée), ou immédiatement sur 401.
	refreshSkew = 3 * time.Minute
)

// AuthError signale un refresh révoqué ou shouldLogout — le dashboard doit
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
	refresh string
	exp     time.Time
}

// authenticator gère le refresh OAuth et le JWT d'accès en mémoire.
type authenticator struct {
	http      *http.Client
	oauthURL  string
	clientID  string
	mu        sync.Mutex
	bundle    tokenBundle
	now       func() time.Time
	onRotated func(newRefresh string)
}

func newAuthenticator(httpClient *http.Client, refreshToken, oauthURL string) *authenticator {
	if oauthURL == "" {
		oauthURL = defaultOAuthURL
	}
	return &authenticator{
		http:     httpClient,
		oauthURL: oauthURL,
		clientID: oauthClientID,
		bundle:   tokenBundle{refresh: strings.TrimSpace(refreshToken)},
		now:      time.Now,
		onRotated: func(newRefresh string) {
			log.Printf("Cursor: nouveau refresh_token reçu — mettre à jour Infisical (ex. /agents/cursor) ; %s",
				Fingerprint(newRefresh))
		},
	}
}

// accessToken renvoie un access JWT valide, en refreshant si besoin.
func (a *authenticator) accessToken() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.bundle.refresh == "" {
		return "", &AuthError{Msg: "CURSOR_REFRESH_TOKEN absent"}
	}
	if a.bundle.access != "" && a.now().Add(refreshSkew).Before(a.bundle.exp) {
		return a.bundle.access, nil
	}
	return a.refreshLocked()
}

// invalidateAccess force un refresh au prochain appel (ex. après 401).
func (a *authenticator) invalidateAccess() {
	a.mu.Lock()
	a.bundle.access = ""
	a.bundle.exp = time.Time{}
	a.mu.Unlock()
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

	if or.logout() {
		log.Printf("Cursor: shouldLogout=true — CURSOR_REFRESH_TOKEN %s", Fingerprint(a.bundle.refresh))
		return "", &AuthError{
			Msg: "authentification Cursor révoquée (shouldLogout) : renouveler CURSOR_REFRESH_TOKEN " +
				"(SQLite cursorAuth/refreshToken ou `agent login`) et mettre à jour Infisical",
		}
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		log.Printf("Cursor: oauth refusé (HTTP %d) — CURSOR_REFRESH_TOKEN %s", resp.StatusCode, Fingerprint(a.bundle.refresh))
		return "", &AuthError{
			Msg: fmt.Sprintf(
				"token Cursor refusé (HTTP %d) : refresh expiré ou révoqué — vérifier CURSOR_REFRESH_TOKEN "+
					"(longueur et empreinte SHA-256 dans les logs du pod)",
				resp.StatusCode),
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
		return "", &AuthError{Msg: "oauth/token: access_token absent — renouveler CURSOR_REFRESH_TOKEN"}
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

func (o oauthResponse) logout() bool {
	if o.ShouldLogout != nil && *o.ShouldLogout {
		return true
	}
	if o.ShouldLogoutSnake != nil && *o.ShouldLogoutSnake {
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
