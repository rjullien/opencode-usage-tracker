# opencode-usage-tracker

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
![Public](https://img.shields.io/badge/status-public-2ea44f)

Suivi des **quotas serveur OpenCode Go** (rolling 5h / weekly / monthly) pour plusieurs clés API.

Deux outils complémentaires dans ce repo :

| Outil | Description | Stack |
|-------|-------------|-------|
| 🖥️ **Dashboard web** | Suivi multi-clés avec barres de progression | Go (binaire unique, `FROM scratch`) |
| 🖥️ **CLI `opencode_usage.py`** | Quotas instantanés en terminal, sortie JSON pour cron | Python 3 stdlib pure |

## 🖥️ Dashboard web

Service Go unique, image Docker `FROM scratch`, déployable n'importe où.

**URL** : https://oc-board.bapttf.com (protégé par Authelia, accès famille)

### Où on en est vs le reset

Un pourcentage brut ne dit rien : 60% consommé est une alerte au 3ᵉ jour et un non-événement
à 3 jours du reset. Le dashboard compare donc le **consommé** au **temps écoulé** dans la période.

La barre porte deux informations : le remplissage est le consommé, le trait vertical marque le
temps écoulé. Remplissage au-delà du trait = on brûle plus vite que le temps ne passe.

La métrique principale est le nombre de **jours à sec** — les jours passés au plafond avant le
reset, au rythme moyen actuel :

```
rythme       = consommé / jours écoulés
mur          = jours restants avant d'atteindre 100%
jours à sec  = (jours jusqu'au reset) − (jours jusqu'au mur)
```

| Feu | Règle |
|-----|-------|
| 🟢 dans le budget | 0 jour à sec, le quota tient jusqu'au reset |
| 🟠 juste | jusqu'à 3 jours à sec, **ou** 80% déjà consommé |
| 🔴 dérapage | plus de 3 jours à sec, **ou** 95% déjà consommé |

Deux garde-fous :

- **Début de période.** Sous 5% de période écoulée (~1,5 j sur un mois), une seule grosse session
  projette un dépassement délirant. Le rouge issu du rythme est plafonné à orange ; le seuil
  absolu peut toujours forcer le rouge.
- **Fin de période.** Le calcul en jours se comprime mécaniquement vers zéro quand il ne reste
  qu'un jour. Les seuils absolus (80% / 95%) portent alors l'alerte à eux seuls.

Le **rolling 5h** est noté sur le consommé brut : à 90% on est bloqué tout de suite, la notion de
rythme n'y a pas de sens. L'API renvoie d'ailleurs `resetsAt = now + 5h` quand la consommation est
nulle, donc aucun début de période n'y est calculable.

### Le feu global

Le feu de l'ensemble est celui du **maillon faible**, pas une moyenne. Les quotas sont propres à
chaque abonnement et ne se transfèrent pas : les points restants d'une clé ne peuvent pas servir à
une autre. Les périodes sont en plus déphasées (une à 12% écoulée à côté d'une à 76%), ce qui rend
toute projection agrégée dénuée de sens. Le consommé moyen reste affiché, mais à titre indicatif.

### Fenêtres de quota

Chaque fenêtre a sa propre règle de début de période, établie sur les réponses réelles de l'API :

| Fenêtre | Début de période | Observation |
|---------|------------------|-------------|
| `monthly` | `resetsAt − 1 mois` | anniversaire d'abonnement : jour et heure arbitraires, propres à chaque clé |
| `weekly` | `resetsAt − 7 jours` | calendaire : toutes les clés partagent le même lundi 00:00 UTC |
| `rolling` | — | réellement glissante, aucun début dérivable |

### Limite connue

Le rythme est la **moyenne depuis le début de la période**, pas un rythme récent : le service ne
conserve aucun historique. Une grosse semaine passée continue de peser sur la projection plusieurs
jours après l'arrêt. Un vrai rythme glissant demanderait de la persistance.

## Architecture

```
┌──────────────┐      GET /zen/go/v1/usage       ┌────────────────┐
│  Dashboard   │ ───── Bearer sk-xxx ────────────▶│  opencode.ai   │
│  (Go binary) │ ◀───── JSON quotas ─────────────│                │
│  :8080       │     fetch à la demande (on-view) │                │
└──────────────┘                                  └────────────────┘
       │
       │ HTTP
       ▼
   Navigateur
```

- **Fetch on-demand** : l'API n'est appelée que quand quelqu'un ouvre la page
- **Cache court** (30s par défaut) pour éviter de spammer l'API sur refresh rapides
- **Pas de polling background** — zéro requête quand personne ne regarde
- **Zéro dépendance** — stdlib Go + embed pour les templates
- **Sans état** — le budget est dérivé à chaque requête de `percent` et `resetsAt`, rien n'est stocké

## Endpoints

| Route | Description |
|-------|-------------|
| `GET /` | Dashboard HTML : feu par abonnement + feu global + section Devin (optionnelle) |
| `GET /api/usage` | JSON des quotas OpenCode, enrichi du budget calculé |
| `GET /api/devin` | JSON du quota ACU Devin (404 si non configuré) |
| `GET /health` | Health check (`{"status":"ok"}`) |

`/api/usage` conserve les champs existants (`label`, `windows`, `error`, `fetchedAt`, et par fenêtre
`name`, `status`, `percent`, `resetsAt`) et ajoute `level` ainsi qu'un objet `budget` :

```json
{
  "name": "Monthly", "kind": "monthly", "percent": 28, "level": "red",
  "budget": {
    "valid": true, "periodDays": 31, "elapsedPct": 13, "consumedPct": 28,
    "aheadPct": 15, "ratePerDay": 7.19, "allowedRatePerDay": 2.66,
    "projectedPct": 223, "dryDays": 17.1, "earlyPeriod": false, "level": "red"
  }
}
```

`budget.valid` est `false` pour le rolling 5h, seule fenêtre sans début de période calculable.

## Variables d'environnement

| Variable | Défaut | Description |
|----------|--------|-------------|
| `PORT` | `8080` | Port d'écoute |
| `CACHE_TTL` | `30s` | Durée du cache anti-spam |
| `BIFROST_URL` | `http://bifrost.openclaw.svc.cluster.local:8080` | Base URL du gateway Bifrost (lecture seule des poids de routage) |
| `OPENCODE_GO_API_KEY` | — | Clé Go, affichée « Main » (au moins une clé requise) |
| `OPENCODE_GO_API_KEY_<SUFFIXE>` | — | Clé supplémentaire, affichée « SUFFIXE » |
| `DEVIN_API_KEY` | — | Token Devin (optionnel) — active la **section ACU Devin**, totalement séparée du lot OpenCode |
| `DEVIN_ORG_ID` | — | Organisation Devin (`org-...`) à interroger, optionnelle : indispensable quand le token est un PAT dont `/v3/self` ne renvoie pas d'`org_id` |
| `DEVIN_RESET_DAY` | `5` | Jour du mois du reset de budget Devin (plan Pro individuel, non exposé par l'API). Seules les valeurs `1`–`28` sont retenues : au-delà, tous les mois n'ont pas ce jour, et la variable retombe sur le défaut `5` |

Toute variable commençant par `OPENCODE_GO_API_KEY` est découverte automatiquement, et le label
d'affichage est déduit du suffixe : `OPENCODE_GO_API_KEY_R` s'affiche « R »,
`OPENCODE_GO_API_KEY_ALICE` s'affiche « Alice ». Ajouter ou retirer un abonnement ne demande
aucune modification de code. L'ordre d'affichage suit le nom de la variable, pour rester stable.

### Section Devin ACU (à part du lot OpenCode)

Sans `DEVIN_API_KEY`, rien ne change : la section Devin est absente du dashboard et
`/api/devin` répond 404. Avec un token `cog_` (PAT ou service user), une carte
« Devin — ACU » apparaît **en dessous** de la grille OpenCode : cycle de facturation
couvert, total ACU consommés, jours relevés, répartition par produit
(devin/cascade/terminal), org. Les 4 clés OpenCode partagées ne sont jamais
mélangées à Devin, et un échec Devin n'empêche jamais le rendu de la grille OpenCode.

**API publique utilisée (conforme à la spec v3, https://docs.devin.ai/v3-openapi.yaml) :**
- `GET /v3/self` → identité du principal (`principal_type`, `user_id`, `api_key_id`…) et
  `org_id` **quand il y en a un**
- `GET /v3/organizations/{org_id}/consumption/daily?time_after=…&time_before=…` →
  `{total_acus, consumption_by_date[{date, acus, acus_by_product}]}`

**Résolution de l'organisation.** `org_id` est `nullable` et absent des champs requis de
`PatUserSelf` comme de `ServiceUserSelf` : un PAT parfaitement valide peut ne porter
aucune organisation, et aucun endpoint public ne permet de lister les organisations d'un
PAT (seul `/v3/enterprise/organizations` existe, hors de portée d'un plan Pro). L'ordre de
résolution est donc `DEVIN_ORG_ID` **puis** l'`org_id` de `/v3/self`. Si les deux sont
vides, la section Devin s'affiche en erreur avec un message qui nomme `DEVIN_ORG_ID` et le
`principal_type` reçu. L'identifiant `org-...` se lit dans l'URL de l'app Devin
(ou dans la réponse d'un token de service user d'organisation).

**À faire au déploiement.** Le code rend le cas diagnosticable, il ne le devine pas : si le
log de boot affiche `DEVIN_ORG_ID absent (org lue dans /v3/self)` **et** que la section
reste en erreur, c'est que le token ne porte pas d'organisation. Il faut alors fournir
l'org — `ENV DEVIN_ORG_ID=org-...` dans le `Dockerfile` (ligne commentée prête à l'emploi)
puis reconstruire l'image, ou la variable côté déploiement. Aucune valeur n'est inventée
ici, et rien de tout cela n'est obligatoire pour démarrer.

**Fenêtre interrogée.** La requête est explicitement bornée sur le cycle de facturation
courant (`time_after`/`time_before` en secondes Unix) déduit de `DEVIN_RESET_DAY` : sans ces
bornes l'API renvoie sa fenêtre par défaut, qui n'a aucune raison de coïncider avec le cycle
affiché. `time_after` est posé sur la frontière de journée de la facturation, la seule chose
que la spec documente pour cet endpoint : *« Billing cycles use midnight PST (Pacific
Standard Time) as the day boundary, which corresponds to 08:00:00 UTC »* — minuit PST, soit
**08:00:00 UTC**, décalage fixe, jamais PDT. (La consigne « pass Unix timestamps that align
with this timezone offset » que l'on lit parfois citée ne concerne **pas** cet endpoint :
elle n'apparaît que sur les variantes `/v3/enterprise/consumption/daily/...`, d'autres
opérations de scope enterprise.) `time_before`, lui, est **volontairement non aligné** : il
est plafonné à l'instant courant, parce que la fin du cycle est dans le futur et que la spec
documente un `422` sur cet endpoint sans rien dire des bornes futures. La ligne
« cycle du … au … » et la date de **reset budget** restent les bornes du cycle, que
`/api/devin` expose (`cycleStart`, `cycleEnd`, absents quand le fetch a échoué).

⚠️ Reste à recouper en production, en comparant le total affiché à l'UI Devin au premier
passage de cycle :
- **Instant des clés `date`.** La spec ne dit pas à quel instant de la journée les clés
  `date` de `consumption_by_date` sont posées. Si elles tombaient à minuit UTC et non à
  08:00 UTC, le total serait décalé d'une journée par rapport au libellé du cycle.
- **Jour en cours et plafonnement de `time_before`.** L'endpoint renvoie des seaux
  journaliers, pas des évènements : si le serveur compare `time_before` à la clé `date` du
  seau (posée à 08:00 UTC), le seau du jour en cours est compté ; s'il exige une journée
  close, la consommation du jour **sort du total** jusqu'au lendemain. Borne alignée et
  borne non future sont incompatibles tant que le cycle est en cours ; le choix fait ici est
  de ne jamais envoyer de borne future, donc d'accepter ce sous-comptage éventuel du jour
  courant.

**Diagnostic.** Le token est trimmé avant usage (un secret monté depuis Kubernetes porte
souvent un `\n` final, qui produisait un 401 interprété à tort comme un token expiré). Les
**logs du pod** (boot et rejets 401/403) portent la longueur et une empreinte SHA-256
tronquée de la clé (`len=44 sha256=1a2b3c4d`), jamais la clé elle-même. Le dashboard et
`/api/devin` étant servis sans authentification, le message qui y apparaît reste
diagnostique mais sans empreinte : il nomme la variable à vérifier et renvoie aux logs.
Les erreurs distinguent la clé de l'organisation : un `401` (ou un `403` sur `/v3/self`)
accuse `DEVIN_API_KEY` ; un `403` ou un `404` sur `/v3/organizations/{org}/…` cite l'org
réellement interrogée et nomme sa provenance — `DEVIN_ORG_ID` quand la variable est
renseignée, `/v3/self` sinon (dans ce dernier cas l'opérateur n'a jamais posé la variable :
c'est le scope du token ou l'état de l'organisation qu'il déclare qu'il faut regarder).

⚠️ L'API publique n'expose **pas** la limite ACU du plan (`acu_limit`,
`daily_quota_remaining_percent`) : elle ne vit que dans le gRPC interne du CLI.
Le board affiche la **consommation réelle**, pas un pourcentage. Les vraies bornes de
cycle ne sont exposées que par `/v3/enterprise/consumption/cycles` (scope enterprise), d'où
le calcul local à partir de `DEVIN_RESET_DAY` (défaut 5, valeur du plan Pro individuel).

## Développement local

```bash
export OPENCODE_GO_API_KEY="sk-..."
go run ./cmd/dashboard
# → http://localhost:8080
```

## Docker

```bash
docker build -t opencode-dashboard .
docker run -p 8080:8080 \
  -e OPENCODE_GO_API_KEY="sk-..." \
  opencode-dashboard
```

Image : `ghcr.io/rjullien/opencode-usage-tracker:main`

## CI/CD

GitHub Actions (`.github/workflows/build.yml`) :

1. **`test`** — `go test ./...`
2. **`smoke`** — construit l'image de runtime, la démarre et vérifie `/health` puis l'heure rendue.
   Ce job attrape ce que les tests unitaires ne peuvent structurellement pas voir : l'image
   `FROM scratch` n'a pas de `/usr/share/zoneinfo`, et le `zoneinfo.zip` livré avec la toolchain Go
   masque un import `time/tzdata` manquant pendant `go test`.
3. **`build`** — build Docker multi-arch (amd64 + arm64) et push sur
   `ghcr.io/rjullien/opencode-usage-tracker`, tags `main`, `vX.Y.Z`, SHA court.

Le binaire est compilé pour l'architecture ciblée via `TARGETARCH` fourni par buildx.

### Déploiement

L'image `:main` est suivie par ArgoCD Image Updater (`newest-build`) dans `BaptTF/vps-infra` :
un merge sur `main` déclenche le build puis le déploiement, sans intervention sur les manifestes.

## 🖥️ CLI `opencode_usage.py`

Quotas instantanés en terminal, sans dépendance (Python 3 stdlib). Scrape l'état SSR de la
page workspace opencode.ai (aucune API publique n'existe encore — cf. issue #31084).

```bash
# cookie inline (le plus simple)
python3 opencode_usage.py -w <WORKSPACE_ID> -c "Fe26.2*..."

# fichier cookie (format Netscape ou ligne simple `auth=...`)
python3 opencode_usage.py -w <WORKSPACE_ID> -f ~/.config/opencode/cookies.txt

# auto-détection du cookie Firefox (profil par défaut)
python3 opencode_usage.py -w <WORKSPACE_ID>

# JSON brut pour scripting / cron
python3 opencode_usage.py -w <WORKSPACE_ID> --json
```

Variables d'environnement : `OPENCODE_WORKSPACE_ID`, `OPENCODE_COOKIE`.

Sortie : barres colorées `⏱ rolling 5h / 📅 weekly / 🗓 monthly` avec % et heure de reset.

⚠️ Le cookie de session expire — ré-auth sur opencode.ai puis re-copier la valeur `auth`.

## License

MIT
