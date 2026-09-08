package devin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// API Devin — endpoints publics utilisés (v3, token PAT ou service user `cog_`) :
//
//	GET /v3/self                                  → identité (+ org_id éventuel)
//	GET /v3/organizations/{org}/consumption/daily → consommation ACU du cycle
//
// Réponses telles que décrites par la spec officielle v3 :
//
//	GET /v3/self → anyOf de 4 schémas (ServiceUserSelf, PatUserSelf,
//	DevinBrainUserSelf, WindsurfSessionUserSelf), par exemple :
//	  {"principal_type":"pat_user","user_id":"user-...","user_name":"...",
//	   "api_key_id":"...","api_key_name":"...","org_id":"org-..."}
//
//	GET /v3/organizations/{org}/consumption/daily?time_after=&time_before= →
//	  {"total_acus":0.0,"consumption_by_date":[
//	    {"date":1788393600,"acus":12.5,
//	     "acus_by_product":{"devin":12.5,"cascade":0,"terminal":0,"review":null}}
//	  ]}
//
// Deux pièges que ce client doit absorber, appris en production :
//
//  1. `org_id` est nullable ET absent des `required` de PatUserSelf comme de
//     ServiceUserSelf : un PAT parfaitement valide peut ne porter AUCUNE
//     organisation. Aucun endpoint public ne permet de lister les organisations
//     d'un PAT (seul /v3/enterprise/organizations existe, hors de portée d'un
//     plan Pro), donc l'org doit alors venir de la configuration : DEVIN_ORG_ID.
//  2. Sans time_after/time_before, l'endpoint renvoie la fenêtre par défaut du
//     serveur, qui n'a aucune raison de coïncider avec le cycle de facturation
//     affiché par le dashboard. On borne donc explicitement la requête sur le
//     cycle courant (voir CycleBounds).
//
// ⚠️ La LIMITE ACU (`acu_limit`, `daily_quota_remaining_percent`...) n'est PAS
// exposée par l'API REST publique : elle ne vit que dans le gRPC interne du CLI
// (GetUserStatus). Le board affiche donc la CONSOMMATION ACU réelle.

const apiBaseURL = "https://api.devin.ai"

const (
	selfPath        = "/v3/self"
	consumptionPath = "/v3/organizations/%s/consumption/daily"
)

// defaultResetDay est le jour du mois où le budget du plan Pro individuel
// repart à zéro (valeur constatée dans l'UI, non exposée par l'API REST).
const defaultResetDay = 5

// cycleBoundaryHourUTC est l'heure UTC de la frontière de journée utilisée par
// la facturation Devin. La spec v3 le dit explicitement pour les endpoints de
// consommation : « Billing cycles use midnight PST as the day boundary, which
// corresponds to 08:00:00 UTC ». C'est un décalage FIXE (PST, jamais PDT), d'où
// une constante et non un fuseau chargé dynamiquement.
const cycleBoundaryHourUTC = 8

// SelfResponse est la réponse de GET /v3/self. Tous les champs sont optionnels
// côté spec (la réponse est un anyOf de 4 schémas) : ne rien supposer présent.
// APIKeyID/APIKeyName servent uniquement au diagnostic.
type SelfResponse struct {
	PrincipalType string `json:"principal_type"`
	UserID        string `json:"user_id"`
	UserName      string `json:"user_name"`
	OrgID         string `json:"org_id"`
	APIKeyID      string `json:"api_key_id"`
	APIKeyName    string `json:"api_key_name"`
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
	// ACUConsumed : total ACU consommés sur le cycle de facturation interrogé
	// (bornes CycleStart/CycleEnd), et non sur la fenêtre par défaut du serveur.
	ACUConsumed float64 `json:"acuConsumed"`

	// Days : consommation par jour (date epoch, acus, acusByProduct).
	Days []DayUsage `json:"days,omitempty"`

	// DayCount : nombre de jours de consommation renvoyés.
	DayCount int `json:"dayCount"`

	// OrgID : organisation interrogée (DEVIN_ORG_ID sinon /v3/self).
	OrgID string `json:"orgId,omitempty"`

	// CycleStart / CycleEnd : bornes du cycle de facturation effectivement
	// envoyées en time_after / time_before. Exposées pour que le dashboard
	// affiche exactement la fenêtre interrogée, sans la recalculer.
	CycleStart time.Time `json:"cycleStart"`
	CycleEnd   time.Time `json:"cycleEnd"`

	FetchedAt time.Time `json:"fetchedAt"`
	Error     string    `json:"error,omitempty"`
}

// DayUsage est un jour de consommation ACU.
type DayUsage struct {
	Date          time.Time     `json:"date"`
	ACUs          float64       `json:"acus"`
	ACUsByProduct ACUsByProduct `json:"acusByProduct"`
}

// Config regroupe tout ce dont le client a besoin. Une struct plutôt qu'une
// liste de paramètres positionnels : token, org et jour de reset ont tous le
// même type primitif, les confondre à l'appel serait silencieux.
type Config struct {
	// Token : DEVIN_API_KEY. Vide → source Devin désactivée.
	Token string
	// OrgID : DEVIN_ORG_ID, prioritaire sur l'org_id de /v3/self. Indispensable
	// quand le PAT ne porte pas d'organisation.
	OrgID string
	// ResetDay : jour du mois du reset de budget, clampé à [1,28].
	ResetDay int
	// Timeout du client HTTP.
	Timeout time.Duration
	// BaseURL : vide → api.devin.ai (renseigné par les tests).
	BaseURL string
}

// ConfigFromEnv lit la configuration Devin de l'environnement. Aucune variable
// n'est obligatoire : sans DEVIN_API_KEY la section Devin est simplement absente.
func ConfigFromEnv(timeout time.Duration) Config {
	return Config{
		Token:    TokenFromEnv(),
		OrgID:    strings.TrimSpace(os.Getenv("DEVIN_ORG_ID")),
		ResetDay: ResetDayFromEnv(),
		Timeout:  timeout,
	}
}

// Client appelle l'API Devin.
type Client struct {
	http     *http.Client
	baseURL  string
	token    string
	orgID    string
	resetDay int

	// now est injectable pour rendre les bornes de cycle déterministes en test
	// (même principe que handler.Handler.now).
	now func() time.Time
}

// NewClient crée un client Devin à partir de sa configuration.
func NewClient(cfg Config) *Client {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = apiBaseURL
	}
	return &Client{
		http:    &http.Client{Timeout: cfg.Timeout},
		baseURL: baseURL,
		// Défense en profondeur : un secret Infisical → Kubernetes porte très
		// souvent un \n final, et « Bearer cog_xxx\n » se traduit par un 401
		// que l'on interprétait à tort comme un token expiré.
		token:    strings.TrimSpace(cfg.Token),
		orgID:    strings.TrimSpace(cfg.OrgID),
		resetDay: clampResetDay(cfg.ResetDay),
		now:      time.Now,
	}
}

// FetchStatus récupère la consommation ACU Devin : /v3/self valide le token et
// fournit l'identité (et l'org quand il y en a une), puis consumption/daily est
// interrogé sur l'organisation résolue, borné au cycle de facturation courant.
func (c *Client) FetchStatus() (Status, error) {
	self, err := c.getJSON(selfPath)
	if err != nil {
		return Status{}, err
	}
	var sr SelfResponse
	if err := json.Unmarshal(self, &sr); err != nil {
		return Status{}, fmt.Errorf("JSON parse error (/v3/self): %w", err)
	}

	orgID, err := c.resolveOrg(sr)
	if err != nil {
		return Status{}, err
	}

	start, end := CycleBounds(c.now(), c.resetDay)
	body, err := c.getJSON(consumptionURL(orgID, start, end))
	if err != nil {
		return Status{}, err
	}
	return parseResponse(body, orgID, start, end)
}

// resolveOrg choisit l'organisation à interroger : la configuration d'abord,
// l'org_id de /v3/self ensuite. Aucun endpoint public ne permet de découvrir
// l'organisation d'un PAT, donc en dernier recours il faut le dire clairement
// plutôt que de laisser un « aucun org_id » énigmatique dans les logs.
func (c *Client) resolveOrg(sr SelfResponse) (string, error) {
	if c.orgID != "" {
		return c.orgID, nil
	}
	if org := strings.TrimSpace(sr.OrgID); org != "" {
		return org, nil
	}
	principal := sr.PrincipalType
	if principal == "" {
		principal = "inconnu"
	}
	return "", fmt.Errorf(
		"aucune organisation Devin résoluble : /v3/self (principal_type=%s) ne renvoie pas de org_id — "+
			"c'est légitime, org_id est nullable et hors des champs requis pour un PAT dans la spec v3, "+
			"et l'API publique n'expose aucun endpoint pour lister les organisations d'un PAT. "+
			"Renseigner DEVIN_ORG_ID=org-... (identifiant visible dans l'URL de l'app Devin) "+
			"ou utiliser un token de service user rattaché à l'organisation",
		principal)
}

// consumptionURL borne la requête sur le cycle de facturation : sans
// time_after/time_before, l'API renvoie sa fenêtre par défaut, qui ne
// correspond pas au cycle affiché par le dashboard.
func consumptionURL(orgID string, start, end time.Time) string {
	q := url.Values{}
	q.Set("time_after", strconv.FormatInt(start.Unix(), 10))
	q.Set("time_before", strconv.FormatInt(end.Unix(), 10))
	return fmt.Sprintf(consumptionPath, url.PathEscape(orgID)) + "?" + q.Encode()
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

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// L'empreinte (jamais le token) laisse une trace exploitable dans les
		// logs du pod : elle permet de comparer la clé réellement envoyée à
		// celle du secret sans jamais l'exposer.
		fp := Fingerprint(c.token)
		log.Printf("Devin: token refusé (HTTP %d) sur %s — DEVIN_API_KEY %s", resp.StatusCode, path, fp)
		return nil, fmt.Errorf(
			"token Devin refusé (HTTP %d) : clé absente, expirée ou hors scope — vérifier DEVIN_API_KEY %s",
			resp.StatusCode, fp)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

// parseResponse convertit la réponse consumption/daily en Status. Les bornes du
// cycle interrogé sont recopiées telles quelles : le dashboard affiche la
// fenêtre réellement demandée, pas une reconstruction.
func parseResponse(body []byte, orgID string, cycleStart, cycleEnd time.Time) (Status, error) {
	var resp ConsumptionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Status{}, fmt.Errorf("JSON parse error: %w", err)
	}

	s := Status{
		ACUConsumed: resp.TotalACUs,
		OrgID:       orgID,
		CycleStart:  cycleStart,
		CycleEnd:    cycleEnd,
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

// CycleBounds renvoie les bornes du cycle de facturation qui contient now :
// start = dernière occurrence du jour resetDay à 08:00:00 UTC qui ne soit pas
// après now, end = l'occurrence suivante (un mois plus tard).
//
// Le calcul est local, et c'est assumé : les vraies bornes de cycle ne sont
// exposées que par /v3/enterprise/consumption/cycles, scope enterprise
// inaccessible à un plan Pro. La frontière de journée est celle documentée par
// la spec pour la consommation (minuit PST = 08:00:00 UTC), ce qui permet
// d'aligner exactement l'affichage et le fenêtrage de la requête.
func CycleBounds(now time.Time, resetDay int) (start, end time.Time) {
	day := clampResetDay(resetDay)
	year, month, _ := now.UTC().Date()

	start = time.Date(year, month, day, cycleBoundaryHourUTC, 0, 0, 0, time.UTC)
	if start.After(now) {
		// La frontière du mois courant n'est pas encore franchie : le cycle en
		// cours a commencé le mois précédent (time.Date normalise month-1).
		start = time.Date(year, month-1, day, cycleBoundaryHourUTC, 0, 0, 0, time.UTC)
	}
	end = time.Date(start.Year(), start.Month()+1, day, cycleBoundaryHourUTC, 0, 0, 0, time.UTC)
	return start, end
}

// Fingerprint résume un token de façon non réversible : « len=44 sha256=1a2b3c4d ».
// La longueur révèle les espaces ou \n parasites, l'empreinte permet de vérifier
// qu'on parle bien de la même clé que celle du secret. Le token brut ne doit
// jamais apparaître dans un log ni dans un message d'erreur.
func Fingerprint(token string) string {
	t := strings.TrimSpace(token)
	if t == "" {
		return "len=0 (aucune clé)"
	}
	sum := sha256.Sum256([]byte(t))
	return fmt.Sprintf("len=%d sha256=%s", len(t), hex.EncodeToString(sum[:])[:8])
}

// TokenFromEnv lit DEVIN_API_KEY (optionnel : dashboard sans Devin si absent).
// Trimmé : un secret monté depuis Kubernetes porte souvent un \n final, et
// « Bearer cog_xxx\n » est refusé par l'API avec un 401 trompeur.
func TokenFromEnv() string {
	return strings.TrimSpace(os.Getenv("DEVIN_API_KEY"))
}

// ResetDayFromEnv lit DEVIN_RESET_DAY : jour du mois du reset de budget Devin.
// Défaut 5 (le plan Pro individuel reset le 5 du mois — valeur constatée dans
// la UI, non exposée par l'API REST). Clampé à [1,28] pour éviter les mois
// sans ce jour.
func ResetDayFromEnv() int {
	if v := strings.TrimSpace(os.Getenv("DEVIN_RESET_DAY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return clampResetDay(n)
		}
	}
	return defaultResetDay
}

// clampResetDay ramène un jour de reset dans [1,28] : au-delà, tous les mois
// n'ont pas ce jour et le cycle sauterait.
func clampResetDay(day int) int {
	if day < 1 || day > 28 {
		return defaultResetDay
	}
	return day
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
