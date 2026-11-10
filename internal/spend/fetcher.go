package spend

import (
	"sync"
	"time"
)

// Fetcher queries Prometheus on demand and keeps the last report for cacheTTL,
// same idea as the OpenCode fetcher: a refresh storm must not stampede Prometheus.
type Fetcher struct {
	client   *Client
	cacheTTL time.Duration

	mu      sync.Mutex
	cache   *Report
	cacheAt time.Time
}

// NewFetcher returns a Fetcher. The client timeout bounds one scrape; the lock
// is held across the scrape so parallel page loads share a single query.
func NewFetcher(client *Client, cacheTTL time.Duration) *Fetcher {
	return &Fetcher{client: client, cacheTTL: cacheTTL}
}

// Report returns the cached snapshot, or a fresh one when the cache has expired.
func (f *Fetcher) Report() *Report {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cache != nil && time.Since(f.cacheAt) < f.cacheTTL {
		return f.cache
	}
	r := f.client.Fetch()
	f.cache = r
	f.cacheAt = time.Now()
	return r
}
