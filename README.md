# opencode-usage-tracker

Dashboard web de suivi des **quotas serveur OpenCode Go** (rolling 5h / weekly / monthly) pour plusieurs clés API.

Service Go unique, image Docker `FROM scratch`, déployable n'importe où.

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

## Personnalisation des labels

Par défaut les clés sont affichées "Key 1", "Key 2", etc. Pour personnaliser, modifier `internal/opencode/keys.go`.

## License

MIT
