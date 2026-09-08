FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS builder

WORKDIR /src

# Cache deps
COPY go.mod ./
RUN go mod download

# Copy source
COPY . .

# Build a static binary for the architecture being targeted. buildx supplies
# TARGETOS/TARGETARCH per platform in the manifest; hardcoding amd64 here used to
# put an amd64 binary inside the arm64 image, which could not execute at all.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build \
    -ldflags="-s -w" \
    -o /dashboard \
    ./cmd/dashboard

# -----------------------------------------------------------
FROM scratch

# TLS root certificates for outgoing HTTPS calls
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Binary
COPY --from=builder /dashboard /dashboard

# Organisation Devin par défaut. `org_id` est nullable pour un PAT : quand
# /v3/self n'en renvoie pas, la section ACU Devin a besoin qu'on la lui donne.
# Le renseigner ici et reconstruire l'image évite de toucher aux manifests de
# déploiement. Laissé commenté : aucune valeur inventée, et rien d'obligatoire
# pour démarrer (sans org résoluble, seule la section Devin s'affiche en erreur).
# ENV DEVIN_ORG_ID=org-xxxxxxxx

EXPOSE 8080

ENTRYPOINT ["/dashboard"]
