package analysisruntime

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
)

const (
	// ContainerStateKeyEnv carries a base64-encoded 256-bit AES key.
	ContainerStateKeyEnv = "PROW_AI_STATE_KEY"
	// ContainerStateMarker prefixes encrypted cache and trace state.
	ContainerStateMarker = "PROW_AI_STATE_B64:"
	// ContainerTaskNamespaceEnv identifies the owning Orka Task.
	ContainerTaskNamespaceEnv = "PROW_AI_TASK_NAMESPACE"
	// ContainerTaskNameEnv identifies the owning Orka Task.
	ContainerTaskNameEnv = "PROW_AI_TASK_NAME"
	// ContainerContractVersionEnv identifies the analyzer contract.
	ContainerContractVersionEnv = "PROW_AI_CONTRACT_VERSION"
	ContainerStateVersion       = 1
	containerStateAAD           = "prow-ai-container-state-v1"
	maxContainerStateBytes      = 512 << 10
	maxContainerCacheSeedBytes  = 48 << 10
)

// ContainerAnalysisState is the encrypted Task-to-dashboard state delta.
type ContainerAnalysisState struct {
	Version      int                      `json:"version"`
	CacheKey     string                   `json:"cache_key"`
	CacheEntries map[string]ai.CacheEntry `json:"cache_entries,omitempty"`
	Traces       []ai.AnalysisTrace       `json:"traces,omitempty"`
}

// FailureCacheKey returns the canonical cache key for one request.
func FailureCacheKey(request ai.FailureAnalysisRequest) string {
	return ai.AgenticCacheKey("universal", request.JobID, request.Build.BuildID, request.TestCase.Name, request.TestCase.FailureMessage)
}

// LoadContainerCacheSeed loads only the cache entry relevant to one request.
func LoadContainerCacheSeed(dataDir string, request ai.FailureAnalysisRequest) map[string]ai.CacheEntry {
	entries := ai.NewCache(dataDir).Entries(FailureCacheKey(request))
	data, err := json.Marshal(entries)
	if err != nil || len(data) > maxContainerCacheSeedBytes {
		return nil
	}
	return entries
}

// RestoreContainerCache writes a bounded Task cache seed before runtime setup.
func RestoreContainerCache(dataDir string, request ai.FailureAnalysisRequest, entries map[string]ai.CacheEntry) error {
	if len(entries) == 0 {
		return nil
	}
	state := ContainerAnalysisState{Version: ContainerStateVersion, CacheKey: FailureCacheKey(request), CacheEntries: entries}
	if err := validateContainerAnalysisState(state); err != nil {
		return err
	}
	cache := ai.NewCache(dataDir)
	cache.Merge(entries)
	return cache.Save()
}

// SnapshotContainerAnalysisState captures one Task's cache entry and traces.
func SnapshotContainerAnalysisState(cache *ai.Cache, traces *ai.TraceStore, request ai.FailureAnalysisRequest) (ContainerAnalysisState, error) {
	key := FailureCacheKey(request)
	state := ContainerAnalysisState{Version: ContainerStateVersion, CacheKey: key, CacheEntries: map[string]ai.CacheEntry{}, Traces: []ai.AnalysisTrace{}}
	if cache != nil {
		state.CacheEntries = cache.Entries(key)
	}
	if traces != nil {
		state.Traces = traces.Snapshot().Traces
	}
	if err := validateContainerAnalysisState(state); err != nil {
		return ContainerAnalysisState{}, err
	}
	return state, nil
}

// ParseContainerStateKey validates one base64-encoded AES-256 key.
func ParseContainerStateKey(raw string) ([]byte, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(raw))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s must be standard base64 for exactly 32 bytes", ContainerStateKeyEnv)
	}
	return key, nil
}

// EncryptContainerAnalysisState encrypts one bounded state delta.
func EncryptContainerAnalysisState(state ContainerAnalysisState, key []byte) (string, error) {
	if len(key) != 32 {
		return "", fmt.Errorf("container state key must be 32 bytes")
	}
	if err := validateContainerAnalysisState(state); err != nil {
		return "", err
	}
	plaintext, err := json.Marshal(state)
	if err != nil {
		return "", fmt.Errorf("marshal container analysis state: %w", err)
	}
	if len(plaintext) > maxContainerStateBytes {
		return "", fmt.Errorf("container analysis state exceeds %d bytes", maxContainerStateBytes)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("create container state nonce: %w", err)
	}
	sealed := gcm.Seal(nil, nonce, plaintext, []byte(containerStateAAD))
	payload := append(nonce, sealed...)
	return base64.StdEncoding.EncodeToString(payload), nil
}

// DecryptContainerAnalysisState decrypts and validates one state payload.
func DecryptContainerAnalysisState(encoded string, key []byte) (ContainerAnalysisState, error) {
	var state ContainerAnalysisState
	if len(key) != 32 {
		return state, fmt.Errorf("container state key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return state, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return state, err
	}
	encoded = strings.TrimSpace(encoded)
	maxPayload := maxContainerStateBytes + gcm.NonceSize() + gcm.Overhead()
	if len(encoded) > base64.StdEncoding.EncodedLen(maxPayload) {
		return state, fmt.Errorf("container analysis state exceeds %d bytes", maxContainerStateBytes)
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return state, fmt.Errorf("decode container analysis state: %w", err)
	}
	if len(payload) > maxPayload {
		return state, fmt.Errorf("container analysis state exceeds %d bytes", maxContainerStateBytes)
	}
	if len(payload) < gcm.NonceSize() {
		return state, fmt.Errorf("container analysis state ciphertext is truncated")
	}
	plaintext, err := gcm.Open(nil, payload[:gcm.NonceSize()], payload[gcm.NonceSize():], []byte(containerStateAAD))
	if err != nil {
		return state, fmt.Errorf("decrypt container analysis state: %w", err)
	}
	if len(plaintext) > maxContainerStateBytes {
		return state, fmt.Errorf("container analysis state exceeds %d bytes", maxContainerStateBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(plaintext))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return state, fmt.Errorf("decode container analysis state JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return state, fmt.Errorf("container analysis state contains multiple JSON values")
	} else if err != io.EOF {
		return state, fmt.Errorf("decode trailing container analysis state data: %w", err)
	}
	if err := validateContainerAnalysisState(state); err != nil {
		return state, err
	}
	return state, nil
}

// WriteEncryptedContainerAnalysisState writes one encrypted state marker.
func WriteEncryptedContainerAnalysisState(w io.Writer, state ContainerAnalysisState, key []byte) error {
	encoded, err := EncryptContainerAnalysisState(state, key)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s%s\n", ContainerStateMarker, encoded); err != nil {
		return fmt.Errorf("write container analysis state: %w", err)
	}
	return nil
}

// ParseEncryptedContainerAnalysisState extracts the last valid encrypted marker.
func ParseEncryptedContainerAnalysisState(raw string, key []byte) (ContainerAnalysisState, error) {
	var (
		state   ContainerAnalysisState
		found   bool
		lastErr error
	)
	for len(raw) > 0 {
		line, rest, cut := strings.Cut(raw, "\n")
		if cut {
			raw = rest
		} else {
			raw = ""
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, ContainerStateMarker) {
			continue
		}
		parsed, err := DecryptContainerAnalysisState(strings.TrimSpace(strings.TrimPrefix(line, ContainerStateMarker)), key)
		if err != nil {
			lastErr = err
			continue
		}
		state = parsed
		found = true
	}
	if found {
		return state, nil
	}
	if lastErr != nil {
		return state, fmt.Errorf("container analysis logs contain no valid state marker: %w", lastErr)
	}
	return state, fmt.Errorf("container analysis state marker is missing")
}

// ContainerStateStore merges Task deltas into shared cache and trace files.
type ContainerStateStore struct {
	mu      sync.Mutex
	dataDir string
	cache   *ai.Cache
	traces  *ai.TraceStore
}

// NewContainerStateStore loads shared cache and trace state.
func NewContainerStateStore(dataDir string) (*ContainerStateStore, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("container state data directory is required")
	}
	traces, err := ai.LoadTraceStore(filepath.Join(dataDir, output.AITraceFilename))
	if err != nil {
		return nil, err
	}
	return &ContainerStateStore{dataDir: dataDir, cache: ai.NewCache(dataDir), traces: traces}, nil
}

// CacheSeed returns the one cache entry relevant to a request.
func (s *ContainerStateStore) CacheSeed(request ai.FailureAnalysisRequest) map[string]ai.CacheEntry {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.cache.Entries(FailureCacheKey(request))
	data, err := json.Marshal(entries)
	if err != nil || len(data) > maxContainerCacheSeedBytes {
		return nil
	}
	return entries
}

// Merge persists one authenticated cache and trace delta.
func (s *ContainerStateStore) Merge(state ContainerAnalysisState) error {
	if s == nil {
		return fmt.Errorf("container state store is required")
	}
	if err := validateContainerAnalysisState(state); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache.Merge(state.CacheEntries)
	for _, trace := range state.Traces {
		s.traces.Upsert(trace)
	}
	return errors.Join(s.cache.Save(), s.traces.Save(filepath.Join(s.dataDir, output.AITraceFilename)))
}

func validateContainerAnalysisState(state ContainerAnalysisState) error {
	if state.Version != ContainerStateVersion {
		return fmt.Errorf("unsupported container analysis state version %d", state.Version)
	}
	if strings.TrimSpace(state.CacheKey) == "" {
		return fmt.Errorf("container analysis state cache key is required")
	}
	if len(state.CacheEntries) > 1 {
		return fmt.Errorf("container analysis state has too many cache entries")
	}
	for key, entry := range state.CacheEntries {
		if key != state.CacheKey || entry.Key != key || entry.CreatedAt.IsZero() || !json.Valid(entry.Data) {
			return fmt.Errorf("container analysis state has an invalid cache entry")
		}
	}
	if len(state.Traces) > 4 {
		return fmt.Errorf("container analysis state has too many traces")
	}
	return nil
}

// RemoveContainerLocalState removes stale local files before seed restoration.
func RemoveContainerLocalState(dataDir string) error {
	var errs []error
	for _, name := range []string{ai.CacheFilename, output.AITraceFilename} {
		if err := os.Remove(filepath.Join(dataDir, name)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
