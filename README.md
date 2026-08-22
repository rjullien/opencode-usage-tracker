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

## Endpoints

| Route | Description |
|-------|-------------|
| `GET /` | Dashboard HTML avec barres de progression |
| `GET /api/usage` | JSON brut des quotas (pour intégration) |
| `GET /health` | Health check (`{"status":"ok"}`) |

## Variables d'environnement

| Variable | Défaut | Description |
|----------|--------|-------------|
| `PORT` | `8080` | Port d'écoute |
| `CACHE_TTL` | `30s` | Durée du cache anti-spam |
| `OPENCODE_GO_API_KEY` | — | Clé Go #1 (requise) |
| `OPENCODE_GO_API_KEY_R` | — | Clé Go #2 (optionnelle) |
| `OPENCODE_GO_API_KEY_A` | — | Clé Go #3 (optionnelle) |
| `OPENCODE_GO_API_KEY_N` | — | Clé Go #4 (optionnelle) |

Au moins une clé doit être définie. Les labels affichés sont "Key 1", "Key 2", etc.

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
- Build Go pour vérification
- Build Docker multi-arch (amd64 + arm64)
- Push sur `ghcr.io/rjullien/opencode-usage-tracker`
- Tags : `main`, `vX.Y.Z`, SHA court

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

## Personnalisation des labels

Par défaut les clés sont affichées "Key 1", "Key 2", etc. Pour personnaliser, modifier `internal/opencode/keys.go`.

## License

MIT
