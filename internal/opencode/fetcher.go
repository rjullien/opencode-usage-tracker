package opencode

import (
	"sync"
	"time"
)

// AgentStatus holds the latest usage data for one agent/key.
type AgentStatus struct {
	Label     string    `json:"label"`
	EnvVar    string    `json:"-"` // env var holding the key; matches Bifrost weight refs
	Windows   []Window  `json:"windows,omitempty"`
	Error     string    `json:"error,omitempty"`
	FetchedAt time.Time `json:"fetchedAt"`
}

// Fetcher fetches usage data on-demand with a short cache to avoid spamming the API.
type Fetcher struct {
	client   *Client
	keys     []KeyConfig
	cacheTTL time.Duration

	mu      sync.RWMutex
	cache   []AgentStatus
	cacheAt time.Time
}

// NewFetcher creates a Fetcher with cache TTL.
func NewFetcher(client *Client, keys []KeyConfig, cacheTTL time.Duration) *Fetcher {
	return &Fetcher{
		client:   client,
		keys:     keys,
		cacheTTL: cacheTTL,
	}
}

// Statuses returns the current usage data, fetching fresh if cache is expired.
func (f *Fetcher) Statuses() []AgentStatus {
	f.mu.RLock()
	if f.cache != nil && time.Since(f.cacheAt) < f.cacheTTL {
		out := make([]AgentStatus, len(f.cache))
		copy(out, f.cache)
		f.mu.RUnlock()
		return out
	}
	f.mu.RUnlock()

	// Fetch fresh data
	results := f.fetchAll()

	f.mu.Lock()
	f.cache = results
	f.cacheAt = time.Now()
	f.mu.Unlock()

	return results
}

func (f *Fetcher) fetchAll() []AgentStatus {
	var wg sync.WaitGroup
	results := make([]AgentStatus, len(f.keys))

	for i, kc := range f.keys {
		wg.Add(1)
		go func(idx int, kc KeyConfig) {
			defer wg.Done()
			windows, err := f.client.FetchUsage(kc.Key)
			status := AgentStatus{
				Label:     kc.Label,
				EnvVar:    kc.EnvVar,
				FetchedAt: time.Now(),
			}
			if err != nil {
				status.Error = err.Error()
			} else {
				status.Windows = windows
			}
			results[idx] = status
		}(i, kc)
	}

	wg.Wait()
	return results
}
