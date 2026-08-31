package devin

import (
	"sync"
	"time"
)

// Fetcher fetches Devin ACU status on-demand with a short cache, same pattern
// as the OpenCode fetcher: never hammer the API on page refreshes.
type Fetcher struct {
	client   *Client
	token    string
	cacheTTL time.Duration

	mu      sync.RWMutex
	cache   *Status
	cacheAt time.Time
}

// NewFetcher crée un Fetcher avec cache TTL. token vide → Statuses() renvoie
// un statut "non configuré" (le dashboard doit rester utilisable sans Devin).
func NewFetcher(client *Client, token string, cacheTTL time.Duration) *Fetcher {
	return &Fetcher{
		client:   client,
		token:    token,
		cacheTTL: cacheTTL,
	}
}

// Statuses returns the current Devin status, fetching fresh if cache expired.
// Renvoie nil quand aucun token n'est configuré : la section Devin du
// dashboard ne s'affiche alors pas (optionnel, totalement à part des 4 clés
// OpenCode partagées).
func (f *Fetcher) Statuses() *Status {
	if f.token == "" {
		return nil
	}

	f.mu.RLock()
	if f.cache != nil && time.Since(f.cacheAt) < f.cacheTTL {
		out := *f.cache
		f.mu.RUnlock()
		return &out
	}
	f.mu.RUnlock()

	s, err := f.client.FetchStatus()
	st := s
	if err != nil {
		st = Status{Error: err.Error(), FetchedAt: time.Now()}
	}

	f.mu.Lock()
	f.cache = &st
	f.cacheAt = time.Now()
	f.mu.Unlock()

	out := st
	return &out
}