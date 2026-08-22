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
| `OPENCODE_GO_API_KEY*` | — | Clés API OpenCode Go (au moins une requise) |

### Clés API — découverte dynamique

Le dashboard détecte **automatiquement** toutes les variables d'environnement qui commencent par `OPENCODE_GO_API_KEY`. Aucune modification de code n'est nécessaire pour ajouter ou supprimer un abonnement.

Exemples :

```bash
OPENCODE_GO_API_KEY=sk-...          # Label affiché : "Main"
OPENCODE_GO_API_KEY_R=sk-...        # Label affiché : "R"
OPENCODE_GO_API_KEY_A=sk-...        # Label affiché : "A"
OPENCODE_GO_API_KEY_ALICE=sk-...    # Label affiché : "ALICE"
```

- Le suffixe après `OPENCODE_GO_API_KEY_` devient le label (en majuscules)
- La clé sans suffixe (`OPENCODE_GO_API_KEY`) a le label "Main"
- Les clés vides ou whitespace-only sont ignorées
- L'ordre d'affichage est alphabétique par nom de variable

Au moins une clé doit être définie.

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

## License

MIT
