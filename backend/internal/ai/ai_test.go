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

func TestQuickSummaryWithMock(t *testing.T) {
	srv := newMockServer(t, `{"summary": "This is a transient throttling error from Azure ARM APIs.", "is_transient": true}`)
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	ctx := context.Background()

	summary, err := client.QuickSummary(ctx, "TestMachineDeployment", "HTTP 429 Too Many Requests", "machine_test.go:42")
	if err != nil {
		t.Fatalf("QuickSummary: %v", err)
	}
	if summary.Summary == "" {
		t.Fatal("expected non-empty summary")
	}
	if !summary.IsTransient {
		t.Error("expected is_transient=true for throttling")
	}
	if summary.GeneratedAt == "" {
		t.Error("expected generated_at to be set")
	}
}

func TestDeepAnalysisWithMock(t *testing.T) {
	jsonResp := `{"root_cause":"Azure quota exceeded","severity":"High","suggested_fix":"Request quota increase","relevant_files":["azure_machine.go"]}`
	srv := newMockServer(t, jsonResp)
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	ctx := context.Background()

	analysis, err := client.DeepAnalysis(ctx, "TestControlPlane", 5, "quota exceeded", "detail body", "log tail", "activity log")
	if err != nil {
		t.Fatalf("DeepAnalysis: %v", err)
	}
	if analysis.RootCause != "Azure quota exceeded" {
		t.Errorf("unexpected root_cause: %q", analysis.RootCause)
	}
	if analysis.Severity != "High" {
		t.Errorf("unexpected severity: %q", analysis.Severity)
	}
	if analysis.SuggestedFix != "Request quota increase" {
		t.Errorf("unexpected suggested_fix: %q", analysis.SuggestedFix)
	}
	if len(analysis.RelevantFiles) != 1 || analysis.RelevantFiles[0] != "azure_machine.go" {
		t.Errorf("unexpected relevant_files: %v", analysis.RelevantFiles)
	}
	if analysis.Model != DeepModel {
		t.Errorf("unexpected model: %q", analysis.Model)
	}
}

func TestCacheHitSkipsAPICall(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"A real bug in CNI configuration."}}]}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	ctx := context.Background()

	// First call — hits API.
	s1, err := client.QuickSummary(ctx, "TestCNI", "calico pods crashing", "cni_test.go:10")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 API call, got %d", callCount)
	}

	// Second call — should use cache.
	s2, err := client.QuickSummary(ctx, "TestCNI", "calico pods crashing", "cni_test.go:10")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected still 1 API call (cache hit), got %d", callCount)
	}

	if s1.Summary != s2.Summary {
		t.Error("cached summary should match original")
	}
}

func TestIsKnownTransient(t *testing.T) {
	cases := []struct {
		msg  string
		want string
	}{
		{"HTTP 429 Too Many Requests", "Azure API throttling (HTTP 429)"},
		{"Azure throttling on resource group", "Azure API throttling (HTTP 429)"},
		{"Too many requests from client", "Azure API throttling (HTTP 429)"},
		{"quota exceeded for StandardDSv3Family", "Azure resource quota exceeded"},
		{"resource quota limit reached", "Azure resource quota exceeded"},
		{"context deadline exceeded during cleanup", "Context deadline during cleanup"},
		{"context deadline exceeded: delete resource group", "Context deadline during cleanup"},
		{"dns resolution failed for mcr.microsoft.com", "DNS resolution failure"},
		{"dns lookup failed for storage.googleapis.com", "DNS resolution failure"},
		{"ImagePullBackOff for calico-node", "Image pull backoff (transient)"},
		{"no space left on device", "Disk space exhausted"},
		{"kubelet certificate expired", ""},
		{"control plane never initialized", ""},
		{"calico-node CrashLoopBackOff", ""},
	}
	for _, tc := range cases {
		got := IsKnownTransient(tc.msg)
		if got != tc.want {
			t.Errorf("IsKnownTransient(%q) = %q, want %q", tc.msg, got, tc.want)
		}
	}
}

func TestComprehensiveAnalysisWithMock(t *testing.T) {
	jsonResp := `{"root_cause":"cloud-init failed to download kubelet binary","severity":"Critical","suggested_fix":"Fix the binary URL in preKubeadmCommands","relevant_files":["templates/cluster-template-prow-azl3.yaml"]}`
	srv := newMockServer(t, jsonResp)
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	ctx := context.Background()

	analysis, err := client.ComprehensiveAnalysis(ctx, Evidence{
		TestName:         "TestAzl3ControlPlane",
		FailureMessage:   "Timed out waiting for control plane",
		FailureBody:      "timeout after 10m",
		ConsecutiveCount: 5,
		BuildLogErrors:   "FATAL: kubeadm init failed",
		BootLog:          "cloud-init: download failed: 404",
		ClusterFlavor:    "prow-azl3",
	})
	if err != nil {
		t.Fatalf("ComprehensiveAnalysis: %v", err)
	}
	if analysis.RootCause != "cloud-init failed to download kubelet binary" {
		t.Errorf("unexpected root_cause: %q", analysis.RootCause)
	}
	if analysis.Severity != "Critical" {
		t.Errorf("unexpected severity: %q", analysis.Severity)
	}
	if analysis.Model != DeepModel {
		t.Errorf("unexpected model: %q", analysis.Model)
	}
	if len(analysis.RelevantFiles) != 1 {
		t.Errorf("unexpected relevant_files: %v", analysis.RelevantFiles)
	}
}

func TestQuickSummaryReturnsAISummaryType(t *testing.T) {
	srv := newMockServer(t, `{"summary": "The kubelet failed to start due to certificate expiration. This is a real bug.", "is_transient": false}`)
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	summary, err := client.QuickSummary(context.Background(), "TestKubelet", "cert expired", "kubelet_test.go:1")
	if err != nil {
		t.Fatal(err)
	}

	// Verify it's the right type and not transient.
	var _ *models.AISummary = summary
	if summary.IsTransient {
		t.Error("cert expiration should not be transient")
	}
}
