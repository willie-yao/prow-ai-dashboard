package tools

import "sync"

// Cache is a small thread-safe key→string map used by tools to memoize
// expensive discovery results across failures of the same build. It is
// intentionally simple: no expiration, no size cap, no eviction. The
// per-build lifecycle means it lives only as long as a single fetcher
// iteration over one build's failures, which bounds it naturally.
//
// Values are typically marshaled JSON tool results. The cache is also
// the right place to memoize "build is not Kubernetes-shaped" so we
// don't re-walk the tree per failure looking for clusters that aren't
// there.
type Cache struct {
	mu    sync.Mutex
	store map[string]string
}

// NewCache returns an empty Cache.
func NewCache() *Cache {
	return &Cache{store: map[string]string{}}
}

// Get returns the stored value and true if present.
func (c *Cache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.store[key]
	return v, ok
}

// Set stores key→value. Last write wins.
func (c *Cache) Set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store[key] = value
}

// Len returns the number of entries. Used by tests + telemetry.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.store)
}
