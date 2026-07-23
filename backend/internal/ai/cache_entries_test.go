package ai

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCacheEntriesCopiesAndMergeUsesNewest(t *testing.T) {
	cache := NewCache("")
	now := time.Now().UTC()
	newer := CacheEntry{Key: "key", CreatedAt: now, Data: json.RawMessage(`{"value":"new"}`)}
	if !cache.Merge(map[string]CacheEntry{"key": newer}) {
		t.Fatal("initial merge did not change cache")
	}
	entries := cache.Entries("key")
	entry := entries["key"]
	entry.Data[0] = '['
	if got := cache.Entries("key")["key"].Data[0]; got != '{' {
		t.Fatal("Entries returned aliased data")
	}
	older := CacheEntry{Key: "key", CreatedAt: now.Add(-time.Minute), Data: json.RawMessage(`{"value":"old"}`)}
	if cache.Merge(map[string]CacheEntry{"key": older}) {
		t.Fatal("older entry replaced cache")
	}
	invalid := map[string]CacheEntry{
		"wrong-key": {Key: "other", CreatedAt: now.Add(time.Minute), Data: json.RawMessage(`{"value":"bad"}`)},
		"bad-json":  {Key: "bad-json", CreatedAt: now.Add(time.Minute), Data: json.RawMessage(`{`)},
	}
	if cache.Merge(invalid) {
		t.Fatal("invalid entries changed cache")
	}
}
