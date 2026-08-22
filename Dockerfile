FROM golang:1.23-alpine AS builder

WORKDIR /src

# Cache deps
COPY go.mod ./
RUN go mod download

# Copy source
COPY . .

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" \
    -o /dashboard \
    ./cmd/dashboard

# -----------------------------------------------------------
FROM scratch

# TLS root certificates for outgoing HTTPS calls
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Binary
COPY --from=builder /dashboard /dashboard

EXPOSE 8080

ENTRYPOINT ["/dashboard"]
