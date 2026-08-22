package main

import (
	"embed"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/rjullien/opencode-usage-tracker/internal/handler"
	"github.com/rjullien/opencode-usage-tracker/internal/opencode"
)

//go:embed templates
var templatesFS embed.FS

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Cache TTL: how long to serve cached results before re-fetching.
	// Prevents spamming the API on rapid page refreshes.
	cacheTTL := 30 * time.Second
	if v := os.Getenv("CACHE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cacheTTL = d
		}
	}

	keys := opencode.LoadKeysFromEnv()
	if len(keys) == 0 {
		log.Fatal("No OpenCode Go API keys configured. Set OPENCODE_GO_API_KEY (or _R, _A, _N variants).")
	}

	log.Printf("Loaded %d API key(s), cache TTL %s", len(keys), cacheTTL)

	client := opencode.NewClient(15 * time.Second)
	fetcher := opencode.NewFetcher(client, keys, cacheTTL)

	h := handler.New(templatesFS, fetcher)

	mux := http.NewServeMux()
	mux.HandleFunc("/", h.Dashboard)
	mux.HandleFunc("/api/usage", h.APIUsage)
	mux.HandleFunc("/health", h.Health)

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	log.Printf("Dashboard listening on :%s", port)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
