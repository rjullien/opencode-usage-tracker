package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/rjullien/opencode-usage-tracker/internal/bifrost"
	"github.com/rjullien/opencode-usage-tracker/internal/devin"
	"github.com/rjullien/opencode-usage-tracker/internal/handler"
	"github.com/rjullien/opencode-usage-tracker/internal/opencode"
)

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

	// Bifrost is an auxiliary, read-only source: the current routing weights
	// per key. A slow or dead gateway must not block the dashboard, so the
	// timeout stays short and failures render as "poids inconnu".
	bifrostURL := os.Getenv("BIFROST_URL")
	if bifrostURL == "" {
		bifrostURL = "http://bifrost.openclaw.svc.cluster.local:8080"
	}
	weights := bifrost.NewClient(bifrostURL, 2*time.Second)
	log.Printf("Bifrost weights source: %s", bifrostURL)

	// Devin ACU : OPTIONNEL et totalement à part des 4 clés OpenCode partagées.
	// Sans DEVIN_API_KEY, la section Devin est absente du dashboard et de l'API.
	var devSrc handler.DevinSource
	if devToken := devin.TokenFromEnv(); devToken != "" {
		devClient := devin.NewClient(15*time.Second, devToken)
		devFetcher := devin.NewFetcher(devClient, devToken, cacheTTL)
		devSrc = devFetcher
		log.Printf("Devin ACU source: configurée (DEVIN_API_KEY présente)")
	} else {
		log.Printf("Devin ACU source: absente (pas de DEVIN_API_KEY)")
	}

	h := handler.New(fetcher, weights, devSrc)

	mux := http.NewServeMux()
	mux.HandleFunc("/", h.Dashboard)
	mux.HandleFunc("/api/usage", h.APIUsage)
	mux.HandleFunc("/api/devin", h.APIDevin)
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
