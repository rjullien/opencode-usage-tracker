package cursor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Fetcher récupère le statut Cursor à la demande avec un cache court, même
// schéma que Devin / OpenCode : jamais spammer l'API sur refresh de page.
type Fetcher struct {
	client   *Client
	token    string
	cacheTTL time.Duration

	mu      sync.RWMutex
	cache   *Status
	cacheAt time.Time
}

// NewFetcher crée un Fetcher. token vide → Statuses() renvoie nil (section absente).
func NewFetcher(client *Client, refreshToken string, cacheTTL time.Duration) *Fetcher {
	return &Fetcher{
		client:   client,
		token:    strings.TrimSpace(refreshToken),
		cacheTTL: cacheTTL,
	}
}

// Statuses renvoie le statut Cursor, en fetchant si le cache a expiré.
// nil quand aucun refresh token n'est configuré.
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

// Fingerprint résume un secret de façon non réversible : « len=44 sha256=1a2b3c4d ».
// Jamais le token brut dans un log ou un message d'erreur servi au navigateur.
func Fingerprint(token string) string {
	t := strings.TrimSpace(token)
	if t == "" {
		return "len=0 (aucun token)"
	}
	sum := sha256.Sum256([]byte(t))
	return fmt.Sprintf("len=%d sha256=%s", len(t), hex.EncodeToString(sum[:])[:8])
}
