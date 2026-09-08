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

// API Devin — deux hôtes :
//
//	REST https://api.devin.ai
//	  GET /v3/self                                  → identité (+ org_id éventuel)
//	  GET /v3/organizations/{org}/consumption/daily → consommation ACU du cycle
//
//	Connect-RPC https://server.codeium.com
//	  POST …/SeatManagementService/GetUserStatus    → quotas jour/semaine (%)
//
// Réponses REST telles que décrites par la spec officielle v3 :
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
// Trois pièges que ce client doit absorber, appris en production :
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
//  3. Les % jour/semaine (dailyQuotaRemainingPercent…) ne sont PAS dans le
//     REST public ni dans /v3/enterprise/consumption/acu-limits (403 hors
//     enterprise). Ils viennent de GetUserStatus ; le même DEVIN_API_KEY
//     (cog_) fonctionne comme apiKey dans le metadata Connect-RPC.
//
// ⚠️ La LIMITE ACU absolue du plan n'est toujours pas dans le REST. Le board
// affiche la consommation ACU + les quotas % jour/semaine de GetUserStatus.

const apiBaseURL = "https://api.devin.ai"

const (
	selfPath        = "/v3/self"
	consumptionPath = "/v3/organizations/%s/consumption/daily"
)

// orgPathPrefix préfixe les endpoints qui portent l'organisation dans leur
// chemin. C'est le seul endroit où un 403 ou un 404 accuse DEVIN_ORG_ID et non
// la clé : « ce token n'a pas accès à cette org » et « cette org n'existe pas »
// sont les deux échecs les plus probables d'un DEVIN_ORG_ID mal renseigné.
const orgPathPrefix = "/v3/organizations/"

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

// Status porte les données Devin affichées par le dashboard : consommation ACU
// (REST api.devin.ai) + quotas jour/semaine (Connect-RPC GetUserStatus).
//
// La limite ACU absolue du plan n'est toujours pas dans le REST public ; en
// revanche GetUserStatus expose daily/weeklyQuotaRemainingPercent — ce sont
// ces pourcentages (retournés en « used ») qui alimentent les barres quota.
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

	// CycleStart / CycleEnd : bornes du cycle de facturation interrogé.
	// CycleStart est exactement le time_after envoyé ; CycleEnd est la fin du
	// cycle, que la requête plafonne à maintenant pour ne pas envoyer de borne
	// future (voir cappedEnd et son arbitrage).
	//
	// Pointeurs, et non des time.Time : sur un statut d'erreur aucune fenêtre
	// n'a été interrogée, et /api/devin doit omettre ces champs plutôt que de
	// publier « 0001-01-01T00:00:00Z » (omitempty est sans effet sur une struct).
	CycleStart *time.Time `json:"cycleStart,omitempty"`
	CycleEnd   *time.Time `json:"cycleEnd,omitempty"`

	// Quota : pourcentages jour/semaine via GetUserStatus. omitempty : si le
	// siège Windsurf est injoignable on garde quand même les ACU REST.
	Quota *Quota `json:"quota,omitempty"`

	// QuotaError : diagnostic soft quand GetUserStatus échoue sans faire
	// échouer toute la carte (les ACU restent affichables).
	QuotaError string `json:"quotaError,omitempty"`

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
	// ResetDay : jour du mois du reset de budget. Hors [1,28] : repli sur le
	// défaut 5 (voir resetDayOrDefault).
	ResetDay int
	// Timeout du client HTTP.
	Timeout time.Duration
	// BaseURL : vide → api.devin.ai (renseigné par les tests).
	BaseURL string
	// SeatURL : vide → server.codeium.com (GetUserStatus). Surchargeable pour
	// les tests et si Devin change d'hôte (credentials.toml api_server_url).
	SeatURL string
}

// ConfigFromEnv lit la configuration Devin de l'environnement. Aucune variable
// n'est obligatoire : sans DEVIN_API_KEY la section Devin est simplement absente.
func ConfigFromEnv(timeout time.Duration) Config {
	return Config{
		Token:    TokenFromEnv(),
		OrgID:    strings.TrimSpace(os.Getenv("DEVIN_ORG_ID")),
		ResetDay: ResetDayFromEnv(),
		Timeout:  timeout,
		SeatURL:  strings.TrimSpace(os.Getenv("DEVIN_API_SERVER")),
	}
}

// Client appelle l'API Devin (REST) et le siège Windsurf (GetUserStatus).
type Client struct {
	http     *http.Client
	baseURL  string
	seatURL  string
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
	seatURL := cfg.SeatURL
	if seatURL == "" {
		seatURL = defaultSeatBaseURL
	}
	return &Client{
		http:    &http.Client{Timeout: cfg.Timeout},
		baseURL: baseURL,
		seatURL: seatURL,
		// Défense en profondeur : un secret Infisical → Kubernetes porte très
		// souvent un \n final, et « Bearer cog_xxx\n » se traduit par un 401
		// que l'on interprétait à tort comme un token expiré.
		token:    strings.TrimSpace(cfg.Token),
		orgID:    strings.TrimSpace(cfg.OrgID),
		resetDay: resetDayOrDefault(cfg.ResetDay),
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

	now := c.now()
	start, end := CycleBounds(now, c.resetDay)
	body, err := c.getJSON(consumptionURL(orgID, start, cappedEnd(end, now)))
	if err != nil {
		return Status{}, err
	}
	// Le Status porte la fin de CYCLE (celle qu'affiche « reset budget »), pas
	// la borne plafonnée envoyée à l'API : la première est la période facturée,
	// la seconde n'est qu'une précaution sur la requête.
	st, err := parseResponse(body, orgID, start, end)
	if err != nil {
		return Status{}, err
	}

	// Quotas jour/semaine : chemin soft. Un échec GetUserStatus ne doit pas
	// masquer les ACU déjà récupérés (REST et siège sont des hôtes distincts).
	if q, qerr := c.fetchQuota(); qerr != nil {
		log.Printf("Devin: quotas GetUserStatus indisponibles : %v", qerr)
		st.QuotaError = qerr.Error()
	} else {
		st.Quota = &q
	}
	return st, nil
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
//
// Ce que la spec v3 documente pour l'endpoint réellement appelé
// (/v3/organizations/{org_id}/consumption/daily) se limite à une phrase : « Get
// daily ACU consumption for the organization. Timezone behavior: Billing cycles
// use midnight PST (Pacific Standard Time) as the day boundary, which
// corresponds to 08:00:00 UTC. » C'est de là, et de rien d'autre, que vient
// l'alignement de time_after sur 08:00 UTC.
//
// La consigne « To match the consumption data shown in the Devin dashboard,
// pass Unix timestamps that align with this timezone offset » ne figure PAS sur
// cet endpoint : elle n'apparaît que sur les variantes
// /v3/enterprise/consumption/daily[/...], qui sont d'autres opérations (scope
// enterprise). Aucune décision de ce fichier ne s'appuie donc sur elle.
//
// La spec ne documente ni l'inclusivité de time_after, ni l'instant exact auquel
// les clés `date` de consumption_by_date sont posées : si ces clés tombaient à
// minuit UTC plutôt qu'à 08:00 UTC, le total serait décalé d'une journée par
// rapport au libellé « cycle du … au … ». Indécidable sans un appel authentifié
// réel (aucun token dans l'environnement de développement) : le total doit être
// recoupé avec l'UI Devin au premier passage de cycle.
func consumptionURL(orgID string, start, end time.Time) string {
	q := url.Values{}
	q.Set("time_after", strconv.FormatInt(start.Unix(), 10))
	q.Set("time_before", strconv.FormatInt(end.Unix(), 10))
	return fmt.Sprintf(consumptionPath, url.PathEscape(orgID)) + "?" + q.Encode()
}

// cappedEnd plafonne la borne haute de la requête à maintenant. La fin du cycle
// courant est par construction dans le futur, la spec documente un 422 sur cet
// endpoint et ne dit rien des bornes futures : plafonner supprime le risque
// qu'une borne future fasse échouer toute la section.
//
// Arbitrage assumé, et son coût : la borne envoyée n'est alors plus alignée sur
// la frontière de journée 08:00 UTC, seule propriété que la spec documente ici.
// L'endpoint renvoie des seaux JOURNALIERS, pas des évènements : si le serveur
// compare time_before à la clé `date` du seau (posée à 08:00 UTC), le seau du
// jour en cours reste dans le total ; s'il exige une journée close, ce seau en
// sort jusqu'au lendemain. Les deux propriétés (borne alignée, borne non future)
// sont incompatibles tant que le cycle est en cours ; le risque retenu est le
// sous-comptage éventuel du jour courant, à recouper avec l'UI Devin au premier
// passage en production (voir README, « Reste à recouper en production »).
func cappedEnd(end, now time.Time) time.Time {
	if now.Before(end) {
		return now
	}
	return end
}

// orgFromPath extrait l'organisation d'un chemin /v3/organizations/{org}/… ,
// et renvoie une chaîne vide quand le chemin n'en porte pas.
func orgFromPath(path string) string {
	p, _, _ := strings.Cut(path, "?")
	rest, ok := strings.CutPrefix(p, orgPathPrefix)
	if !ok {
		return ""
	}
	org, _, _ := strings.Cut(rest, "/")
	if unescaped, err := url.PathUnescape(org); err == nil {
		return unescaped
	}
	return org
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

	switch code := resp.StatusCode; {
	case code == http.StatusOK:
		return body, nil

	case code == http.StatusForbidden || code == http.StatusNotFound:
		// Sur le chemin de l'organisation, un 403 signifie « ce token n'a pas
		// accès à cette org » et un 404 « cette org n'existe pas » : accuser
		// DEVIN_API_KEY enverrait l'opérateur vérifier la mauvaise variable.
		if org := orgFromPath(path); org != "" {
			log.Printf("Devin: organisation %s inaccessible (HTTP %d) — DEVIN_API_KEY %s", org, code, Fingerprint(c.token))
			return nil, c.orgInaccessibleErr(org, code)
		}
		if code == http.StatusNotFound {
			return nil, fmt.Errorf("HTTP %d: %s", code, truncate(string(body), 200))
		}
		return nil, c.tokenRefuseErr(code, path)

	case code == http.StatusUnauthorized:
		return nil, c.tokenRefuseErr(code, path)

	default:
		return nil, fmt.Errorf("HTTP %d: %s", code, truncate(string(body), 200))
	}
}

// orgInaccessibleErr construit le diagnostic d'un 403/404 sur le chemin de
// l'organisation, et distingue les deux provenances possibles de cette org :
//
//   - c.orgID renseigné : l'org vient de DEVIN_ORG_ID, c'est cette variable
//     qu'il faut relire en premier (faute de frappe, org d'un autre compte).
//   - c.orgID vide : l'org a été lue dans /v3/self, donc l'opérateur n'a jamais
//     posé DEVIN_ORG_ID — l'envoyer « vérifier DEVIN_ORG_ID » le renverrait sur
//     une variable absente alors que la cause est le scope du token ou l'état de
//     l'organisation qu'il déclare.
func (c *Client) orgInaccessibleErr(org string, code int) error {
	if c.orgID != "" {
		return fmt.Errorf(
			"organisation Devin %s inaccessible (HTTP %d) : elle n'existe pas ou ce token n'y a pas accès — "+
				"vérifier DEVIN_ORG_ID, ou utiliser un token rattaché à cette organisation",
			org, code)
	}
	return fmt.Errorf(
		"organisation Devin %s inaccessible (HTTP %d) : cette org vient de /v3/self, DEVIN_ORG_ID n'est pas renseigné — "+
			"ce token n'a pas accès à l'organisation qu'il déclare, ou celle-ci n'existe plus ; "+
			"utiliser un token rattaché à une organisation active, ou forcer l'organisation à interroger "+
			"avec DEVIN_ORG_ID=org-...",
		org, code)
}

// tokenRefuseErr construit le diagnostic d'un rejet de la clé elle-même.
//
// L'empreinte (jamais le token) part dans les logs du pod, où elle permet de
// comparer la clé réellement envoyée à celle du secret. Elle ne part PAS dans
// l'erreur retournée : celle-ci finit dans Status.Error, donc dans la page HTML
// et dans /api/devin, tous deux servis sans authentification — la longueur
// exacte de la clé et 32 bits de son empreinte n'ont rien à y faire.
func (c *Client) tokenRefuseErr(code int, path string) error {
	log.Printf("Devin: token refusé (HTTP %d) sur %s — DEVIN_API_KEY %s", code, path, Fingerprint(c.token))
	return fmt.Errorf(
		"token Devin refusé (HTTP %d) : clé absente, expirée ou hors scope — vérifier DEVIN_API_KEY "+
			"(longueur et empreinte SHA-256 de la clé envoyée dans les logs du pod)",
		code)
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
		CycleStart:  &cycleStart,
		CycleEnd:    &cycleEnd,
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
	day := resetDayOrDefault(resetDay)
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
// la UI, non exposée par l'API REST). Une valeur hors [1,28] retombe sur ce
// défaut (voir resetDayOrDefault).
func ResetDayFromEnv() int {
	if v := strings.TrimSpace(os.Getenv("DEVIN_RESET_DAY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return resetDayOrDefault(n)
		}
	}
	return defaultResetDay
}

// resetDayOrDefault n'admet qu'un jour de reset dans [1,28] : au-delà, tous les
// mois n'ont pas ce jour et le cycle sauterait. Une valeur hors bornes retombe
// sur le défaut (5) et NON sur la borne la plus proche : ramener 30 à 28
// donnerait une date de reset fausse tout en ayant l'air d'obéir, alors que le
// défaut est une valeur connue, cohérente avec ce qu'affiche le dashboard quand
// DEVIN_RESET_DAY n'est pas renseigné du tout.
func resetDayOrDefault(day int) int {
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
