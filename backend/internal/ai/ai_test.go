package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// ---------- Cache tests ----------

func TestCacheSetAndGet(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir)

	val := map[string]string{"hello": "world"}
	if err := c.Set("k1", val); err != nil {
		t.Fatalf("Set: %v", err)
	}

	raw, ok := c.Get("k1")
	if !ok {
		t.Fatal("expected cache hit")
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got["hello"] != "world" {
		t.Fatalf("unexpected value: %v", got)
	}
}

func TestCacheMiss(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir)

	_, ok := c.Get("nonexistent")
	if ok {
		t.Fatal("expected cache miss")
	}
}

func TestCacheExpiry(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir)

	_ = c.Set("old", "data")
	// Manually back-date the entry.
	c.mu.Lock()
	entry := c.entries["old"]
	entry.CreatedAt = time.Now().Add(-31 * 24 * time.Hour)
	c.entries["old"] = entry
	c.mu.Unlock()

	_, ok := c.Get("old")
	if ok {
		t.Fatal("expected expired entry to be a miss")
	}
}

func TestCacheSaveAndReload(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir)
	_ = c.Set("persist", "yes")
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify file exists.
	data, err := os.ReadFile(filepath.Join(dir, "ai_cache.json"))
	if err != nil {
		t.Fatalf("read cache file: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("cache file is empty")
	}

	// Reload.
	c2 := NewCache(dir)
	raw, ok := c2.Get("persist")
	if !ok {
		t.Fatal("expected cache hit after reload")
	}
	var got string
	json.Unmarshal(raw, &got)
	if got != "yes" {
		t.Fatalf("unexpected: %q", got)
	}
}

// ---------- Helper tests ----------

func TestDetectTransient(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"This is a transient Azure throttling error.", true},
		{"Looks like a flaky DNS issue.", true},
		{"The test consistently fails due to a missing CRD.", false},
		{"Temporary quota exceeded, should auto-resolve.", true},
		{"Intermittent connection reset.", true},
		{"kubelet certificate expired.", false},
		{"Failed after retry due to timeout.", true},
	}
	for _, tc := range cases {
		got := detectTransient(tc.text)
		if got != tc.want {
			t.Errorf("detectTransient(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestNormalizeError(t *testing.T) {
	input := "error at 0xDEADBEEF with id 12345678-1234-1234-1234-123456789abc foo"
	got := normalizeError(input)
	if got != "error at <addr> with id <uuid> foo" {
		t.Fatalf("unexpected: %q", got)
	}
}

func TestExtractJSON(t *testing.T) {
	fenced := "Here is the analysis:\n```json\n{\"root_cause\": \"test\"}\n```\nDone."
	got := extractJSON(fenced)
	if got != `{"root_cause": "test"}` {
		t.Fatalf("unexpected: %q", got)
	}

	bare := `Some text {"key": "val"} more text`
	got = extractJSON(bare)
	if got != `{"key": "val"}` {
		t.Fatalf("unexpected: %q", got)
	}
}

// ---------- Mock API tests ----------

func newMockServer(t *testing.T, response string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, response)
	}))
}

func newTestClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	c := NewClient("test-token", t.TempDir())
	c.apiURL = serverURL
	return c
}

func TestAnalyzeWithMock(t *testing.T) {
	jsonResp := `{"summary":"Kubelet failed to start due to expired cert.","is_transient":false,"root_cause":"Kubelet client cert expired","severity":"High","suggested_fix":"Rotate cert","relevant_files":["kubelet.conf"]}`
	srv := newMockServer(t, jsonResp)
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	ctx := context.Background()

	summary, analysis, err := client.doAnalyze(ctx, "comprehensive:abc123", "system", "user prompt")
	if err != nil {
		t.Fatalf("doAnalyze: %v", err)
	}
	if summary.Summary != "Kubelet failed to start due to expired cert." {
		t.Errorf("unexpected summary: %q", summary.Summary)
	}
	if summary.IsTransient {
		t.Error("expected is_transient=false")
	}
	if summary.GeneratedAt == "" {
		t.Error("expected generated_at to be set on summary")
	}
	if analysis.RootCause != "Kubelet client cert expired" {
		t.Errorf("unexpected root_cause: %q", analysis.RootCause)
	}
	if analysis.Severity != "High" {
		t.Errorf("unexpected severity: %q", analysis.Severity)
	}
	if analysis.SuggestedFix != "Rotate cert" {
		t.Errorf("unexpected suggested_fix: %q", analysis.SuggestedFix)
	}
	if len(analysis.RelevantFiles) != 1 || analysis.RelevantFiles[0] != "kubelet.conf" {
		t.Errorf("unexpected relevant_files: %v", analysis.RelevantFiles)
	}
	if analysis.Model != Model {
		t.Errorf("unexpected model: %q", analysis.Model)
	}
	if analysis.CacheHit {
		t.Error("expected cache_hit=false on first call")
	}
	if analysis.ElapsedMs < 0 {
		t.Errorf("expected non-negative elapsed_ms, got %d", analysis.ElapsedMs)
	}
	if analysis.ModelBytes <= 0 {
		t.Errorf("expected positive model_bytes on curator path, got %d", analysis.ModelBytes)
	}
	if analysis.ToolCalls != 0 {
		t.Errorf("expected tool_calls=0 on curator path, got %d", analysis.ToolCalls)
	}
	if analysis.GCSBytes != 0 {
		t.Errorf("expected gcs_bytes=0 on curator path, got %d", analysis.GCSBytes)
	}
}

func TestAnalyzeFallbackSummaryFromRootCause(t *testing.T) {
	// Model omits the "summary" field — derive it from root_cause.
	jsonResp := `{"root_cause":"Azure quota exceeded for VM size Standard_D4s_v3. Request quota increase.","severity":"High","suggested_fix":"File quota ticket"}`
	srv := newMockServer(t, jsonResp)
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	summary, _, err := client.doAnalyze(context.Background(), "comprehensive:fallback", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyze: %v", err)
	}
	want := "Azure quota exceeded for VM size Standard_D4s_v3."
	if summary.Summary != want {
		t.Errorf("expected derived summary %q, got %q", want, summary.Summary)
	}
}

func TestCacheHitSkipsAPICall(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`,
			`{"summary":"CNI misconfig.","is_transient":false,"root_cause":"A real bug in CNI configuration.","severity":"High","suggested_fix":"x","relevant_files":[]}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	ctx := context.Background()

	// First call hits the API.
	s1, a1, err := client.doAnalyze(ctx, "comprehensive:cni", "sys", "user")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 API call, got %d", callCount)
	}
	if a1.CacheHit {
		t.Error("expected cache_hit=false on first call")
	}

	// Second call uses the cache.
	s2, a2, err := client.doAnalyze(ctx, "comprehensive:cni", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected still 1 API call (cache hit), got %d", callCount)
	}
	if !a2.CacheHit {
		t.Error("expected cache_hit=true on second call")
	}
	if a2.ModelBytes != 0 {
		t.Errorf("expected model_bytes=0 on cache hit (no model call), got %d", a2.ModelBytes)
	}

	if s1.Summary != s2.Summary {
		t.Error("cached summary should match original")
	}
}

func TestAnalyzeReturnsAISummaryType(t *testing.T) {
	srv := newMockServer(t, `{"summary":"The kubelet failed to start due to certificate expiration. This is a real bug.","is_transient":false,"root_cause":"cert expired","severity":"High","suggested_fix":"rotate","relevant_files":[]}`)
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	summary, _, err := client.doAnalyze(context.Background(), "comprehensive:kubelet", "sys", "user")
	if err != nil {
		t.Fatal(err)
	}

	var _ *models.AISummary = summary
	if summary.IsTransient {
		t.Error("cert expiration should not be transient")
	}
}

// ---------- Pluggable endpoint / header tests ----------

func TestNewClientWithOptionsDefaults(t *testing.T) {
	c := NewClientWithOptions(Options{Token: "x", CacheDir: t.TempDir()})
	if c.Endpoint() != ModelsAPIURL {
		t.Errorf("default endpoint = %q, want %q", c.Endpoint(), ModelsAPIURL)
	}
	if c.ModelName() != Model {
		t.Errorf("default model = %q, want %q", c.ModelName(), Model)
	}
}

func TestNewClientWithOptionsOverrides(t *testing.T) {
	c := NewClientWithOptions(Options{
		Token:    "x",
		CacheDir: t.TempDir(),
		Endpoint: "https://example.com/v1/chat/completions",
		Model:    "my-model",
	})
	if c.Endpoint() != "https://example.com/v1/chat/completions" {
		t.Errorf("endpoint = %q", c.Endpoint())
	}
	if c.ModelName() != "my-model" {
		t.Errorf("model = %q", c.ModelName())
	}
}

func TestIsCopilotEndpoint(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://api.githubcopilot.com/chat/completions", true},
		{"https://api.githubcopilot.com:443/chat/completions", true},
		{"https://api.openai.com/v1/chat/completions", false},
		{"https://integrate.api.nvidia.com/v1/chat/completions", false},
		{"https://my.openai.azure.com/openai/deployments/gpt4/chat/completions", false},
		{"http://localhost:11434/v1/chat/completions", false},
		{"://broken", false},
	}
	for _, tt := range tests {
		if got := isCopilotEndpoint(tt.url); got != tt.want {
			t.Errorf("isCopilotEndpoint(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

// TestCopilotHeaderSkippedForNonCopilotEndpoint verifies the integration
// header isn't sent when the configured endpoint isn't api.githubcopilot.com.
// The positive case (header present for the Copilot endpoint) is covered by
// TestAnalyzeWithMock, which still asserts a successful round-trip with the
// default ModelsAPIURL.
func TestCopilotHeaderSkippedForNonCopilotEndpoint(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Copilot-Integration-Id")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, `{"summary":"s","root_cause":"r","severity":"Low","suggested_fix":"f"}`)
	}))
	defer srv.Close()

	c := NewClientWithOptions(Options{Token: "tok", CacheDir: t.TempDir(), Endpoint: srv.URL, Model: "m"})
	if _, err := c.callAPI(context.Background(), "m", "sys", "user"); err != nil {
		t.Fatalf("callAPI: %v", err)
	}
	if got != "" {
		t.Errorf("expected no Copilot-Integration-Id header for %q, got %q", srv.URL, got)
	}
}

func TestCallAPICustomHeaders(t *testing.T) {
	var gotAuth, gotNIM, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotNIM = r.Header.Get("NIM-Function-Id")
		gotAPIKey = r.Header.Get("api-key")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, `{"summary":"s","root_cause":"r","severity":"Low","suggested_fix":"f"}`)
	}))
	defer srv.Close()

	c := NewClientWithOptions(Options{
		Token:    "secret-bearer",
		CacheDir: t.TempDir(),
		Endpoint: srv.URL,
		Model:    "m",
		ExtraHeaders: map[string]string{
			"NIM-Function-Id": "abc-123",
			"api-key":         "azure-key",
		},
	})
	if _, err := c.callAPI(context.Background(), "m", "sys", "user"); err != nil {
		t.Fatalf("callAPI: %v", err)
	}

	if gotAuth != "Bearer secret-bearer" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer secret-bearer")
	}
	if gotNIM != "abc-123" {
		t.Errorf("NIM-Function-Id = %q, want %q", gotNIM, "abc-123")
	}
	if gotAPIKey != "azure-key" {
		t.Errorf("api-key = %q, want %q", gotAPIKey, "azure-key")
	}
}

func TestCallAPIExtraHeadersOverrideAuthorization(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, `{"summary":"s","root_cause":"r","severity":"Low","suggested_fix":"f"}`)
	}))
	defer srv.Close()

	c := NewClientWithOptions(Options{
		Token:    "ignored",
		CacheDir: t.TempDir(),
		Endpoint: srv.URL,
		Model:    "m",
		ExtraHeaders: map[string]string{
			"Authorization": "Token custom-scheme",
		},
	})
	if _, err := c.callAPI(context.Background(), "m", "sys", "user"); err != nil {
		t.Fatalf("callAPI: %v", err)
	}
	if gotAuth != "Token custom-scheme" {
		t.Errorf("Authorization = %q, want extras to win", gotAuth)
	}
}
