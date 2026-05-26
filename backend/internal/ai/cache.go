package ai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const cacheMaxAge = 30 * 24 * time.Hour

// CacheEntry holds a single cached AI response.
type CacheEntry struct {
	Key       string          `json:"key"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}

// Cache provides a simple file-backed key/value store for AI responses.
type Cache struct {
	dir     string
	mu      sync.Mutex
	entries map[string]CacheEntry
}

// NewCache creates a cache, loading existing entries from dir/ai_cache.json.
func NewCache(dir string) *Cache {
	c := &Cache{
		dir:     dir,
		entries: make(map[string]CacheEntry),
	}
	data, err := os.ReadFile(filepath.Join(dir, "ai_cache.json"))
	if err == nil {
		var entries map[string]CacheEntry
		if json.Unmarshal(data, &entries) == nil {
			c.entries = entries
		}
	}
	return c
}

// Get returns the cached data if the key exists and is not older than 30 days.
func (c *Cache) Get(key string) (json.RawMessage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Since(entry.CreatedAt) > cacheMaxAge {
		delete(c.entries, key)
		return nil, false
	}
	return entry.Data, true
}

// Set stores data under the given key.
func (c *Cache) Set(key string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = CacheEntry{
		Key:       key,
		CreatedAt: time.Now(),
		Data:      raw,
	}
	return nil
}

// Save writes the cache to dir/ai_cache.json.
func (c *Cache) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := json.MarshalIndent(c.entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.dir, "ai_cache.json"), data, 0o644)
}
