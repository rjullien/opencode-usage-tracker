package spend

import (
	"sync"
	"time"
)

// Fetcher queries Prometheus on demand and keeps the last report per window
// for cacheTTL, same idea as the OpenCode fetcher: a refresh storm must not
// stampede Prometheus.
type Fetcher struct {
	client   *Client
	cacheTTL time.Duration

	mu    sync.Mutex
	cache map[string]cachedReport
}

type cachedReport struct {
	report *Report
	at     time.Time
}

// NewFetcher returns a Fetcher. The client timeout bounds one scrape; the lock
// is held across the scrape so parallel page loads share a single query.
func NewFetcher(client *Client, cacheTTL time.Duration) *Fetcher {
	return &Fetcher{client: client, cacheTTL: cacheTTL, cache: map[string]cachedReport{}}
}

// Report returns the cached snapshot for the window, or a fresh one when the
// cache has expired. Unknown windows are the current calendar month.
func (f *Fetcher) Report(window string) *Report {
	window = NormalizeWindow(window)
	f.mu.Lock()
	defer f.mu.Unlock()
	if hit, ok := f.cache[window]; ok && time.Since(hit.at) < f.cacheTTL {
		return hit.report
	}
	r := f.client.Fetch(window)
	f.cache[window] = cachedReport{report: r, at: time.Now()}
	return r
}
