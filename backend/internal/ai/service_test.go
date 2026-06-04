package ai

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/filesystem"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// stubModule satisfies ai.Module for service tests. Behaviors are controlled
// via the exported fields.
type stubModule struct {
	name          string
	prompt        string
	transientFor  string // failure messages exactly matching this string are flagged transient
	transientWith string // the returned reason when transientFor matches
	prefer        bool
	preferReason  string
}

func (m *stubModule) Name() string { return m.name }
func (m *stubModule) IsKnownTransient(msg string) string {
	if msg != "" && msg == m.transientFor {
		return m.transientWith
	}
	return ""
}
func (m *stubModule) AnalysisPrompt(_ context.Context, _ *http.Client, _ *models.BuildResult, _ *models.TestCase, _ int) string {
	return m.prompt
}

// stubPreferrer wraps stubModule and implements AgenticPreferrer.
type stubPreferrer struct {
	*stubModule
}

func (p *stubPreferrer) PrefersAgentic(_ *models.BuildResult, _ *models.TestCase) (bool, string) {
	return p.prefer, p.preferReason
}

func newRun(jobName, buildID string) *models.BuildResult {
	return &models.BuildResult{
		BuildInfo: models.BuildInfo{JobName: jobName, BuildID: buildID},
	}
}

func newFailedTC(name, msg string) *models.TestCase {
	return &models.TestCase{Name: name, FailureMessage: msg, Status: "failed"}
}

// ---------- Mode + cache invalidation ----------

func TestService_AgenticAlways_TagsModeAgentic(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	final := `{"summary":"x","is_transient":false,"root_cause":"y","severity":"Low","suggested_fix":"f","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	s := NewService(client, &stubModule{name: "capi", prompt: "user"}, "sys", nil)
	s.EnableAgentic(AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second}, &fakeFactory{}, registry, enabled, true /* always */, false /* universalPath */)

	tc := newFailedTC("Test A", "failure msg")
	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if tc.AIAnalysis == nil {
		t.Fatal("AIAnalysis nil")
	}
	if tc.AIAnalysis.Mode != AgenticMode {
		t.Errorf("Mode = %q, want %q", tc.AIAnalysis.Mode, AgenticMode)
	}
}

func TestService_ToolsUnsupported_FallsBackOnce(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// First failure: agentic 400 (function calling unsupported).
	srv.push(400, `{"error":{"message":"function calling not supported"}}`)
	// Curator fallback for first failure.
	srv.push(200, chatRespFinal(`{"summary":"first","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"f","relevant_files":[]}`))
	// Second failure: should skip agentic entirely and go straight to curator.
	srv.push(200, chatRespFinal(`{"summary":"second","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"f","relevant_files":[]}`))

	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	s := NewService(client, &stubModule{name: "capi", prompt: "user"}, "sys", nil)
	s.EnableAgentic(AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second}, &fakeFactory{}, registry, enabled, true, false)

	tc1 := newFailedTC("Test A", "msg-a")
	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc1)
	tc2 := newFailedTC("Test B", "msg-b")
	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc2)

	if tc1.AIAnalysis == nil || tc1.AIAnalysis.Mode != curatorMode {
		t.Errorf("first analysis: expected curator fallback after ErrToolsUnsupported, got %+v", tc1.AIAnalysis)
	}
	if tc2.AIAnalysis == nil || tc2.AIAnalysis.Mode != curatorMode {
		t.Errorf("second analysis: expected curator (tools-unsupported flag should stick), got %+v", tc2.AIAnalysis)
	}
	// Expect exactly 3 server calls: 1 agentic 400 + 2 curator successes.
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Errorf("server calls = %d, want 3", got)
	}
}

func TestService_TransientShortCircuit(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// No server pushes: any HTTP call should explode this test.

	client := newAgenticTestClient(t, srv.URL)
	mod := &stubModule{name: "capi", prompt: "user", transientFor: "rate limit", transientWith: "HTTP 429: rate limited"}
	s := NewService(client, mod, "sys", nil)

	tc := newFailedTC("Test A", "rate limit")
	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if tc.AISummary == nil || !tc.AISummary.IsTransient {
		t.Fatal("expected transient summary")
	}
	if tc.AIAnalysis != nil {
		t.Error("transient should NOT produce a deep analysis")
	}
	if got := atomic.LoadInt32(&srv.calls); got != 0 {
		t.Errorf("server calls = %d, want 0 (transient should short-circuit)", got)
	}
}

func TestService_ReanalyzeOnModeChange(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Agentic call returns final JSON; cached "curator" entry should be invalidated.
	final := `{"summary":"new agentic","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"f","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	s := NewService(client, &stubModule{name: "capi", prompt: "user"}, "sys", nil)
	s.EnableAgentic(AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second}, &fakeFactory{}, registry, enabled, true, false)

	// Test case already has CURATOR analysis cached on it from a prior run.
	tc := newFailedTC("Test A", "msg")
	tc.AISummary = &models.AISummary{Summary: "stale curator summary"}
	tc.AIAnalysis = &models.AIAnalysis{RootCause: "stale curator root cause", Mode: curatorMode}

	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if tc.AIAnalysis.Mode != AgenticMode {
		t.Errorf("Mode = %q, want %q (stale curator entry should be re-analyzed under agentic)", tc.AIAnalysis.Mode, AgenticMode)
	}
	if !strings.Contains(tc.AISummary.Summary, "new agentic") {
		t.Errorf("expected fresh agentic summary, got %q", tc.AISummary.Summary)
	}
}

func TestService_SkipWhenAlreadyAnalyzedSameMode(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// No server pushes: should not call the API.

	client := newAgenticTestClient(t, srv.URL)
	s := NewService(client, &stubModule{name: "capi", prompt: "user"}, "sys", nil)

	tc := newFailedTC("Test A", "msg")
	tc.AISummary = &models.AISummary{Summary: "cached"}
	tc.AIAnalysis = &models.AIAnalysis{RootCause: "cached", Mode: curatorMode}

	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if got := atomic.LoadInt32(&srv.calls); got != 0 {
		t.Errorf("server calls = %d, want 0 (existing curator analysis should be kept)", got)
	}
	if tc.AIAnalysis.RootCause != "cached" {
		t.Errorf("expected cached root cause to be preserved, got %q", tc.AIAnalysis.RootCause)
	}
}

// TestService_NormalizesEmptyModeOnLegacyCache covers analyses loaded from
// disk that were written before AIAnalysis.Mode was populated. shouldReanalyze
// treats empty Mode as curator and keeps the cached value; the Analyze
// early-exit must then stamp Mode = "curator" so the next published JSON has a
// uniform non-empty mode for every analysis.
func TestService_NormalizesEmptyModeOnLegacyCache(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	client := newAgenticTestClient(t, srv.URL)
	s := NewService(client, &stubModule{name: "capi", prompt: "user"}, "sys", nil)

	tc := newFailedTC("Test A", "msg")
	tc.AISummary = &models.AISummary{Summary: "cached"}
	tc.AIAnalysis = &models.AIAnalysis{RootCause: "cached"}

	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if got := atomic.LoadInt32(&srv.calls); got != 0 {
		t.Errorf("server calls = %d, want 0 (legacy curator cache should be kept, not re-analyzed)", got)
	}
	if tc.AIAnalysis.Mode != curatorMode {
		t.Errorf("Mode = %q, want %q (legacy empty Mode should be normalized to curator)", tc.AIAnalysis.Mode, curatorMode)
	}
}

func TestService_CacheKeyShape(t *testing.T) {
	s := &Service{module: &stubModule{name: "capi"}}
	// Curator key for "capi" stays in the legacy "comprehensive:<hash>" shape.
	curator := s.cacheKey("Test A", "boom")
	if !strings.HasPrefix(curator, "comprehensive:") {
		t.Errorf("capi curator key should start with 'comprehensive:', got %q", curator)
	}
	// Other module names get the new "analyze:<module>:<hash>" shape.
	s2 := &Service{module: &stubModule{name: "kubernetes"}}
	other := s2.cacheKey("Test A", "boom")
	if !strings.HasPrefix(other, "analyze:kubernetes:") {
		t.Errorf("non-capi curator key should start with 'analyze:kubernetes:', got %q", other)
	}
	// Agentic key encodes job + build so two builds of the same test never collide.
	a1 := s.agenticCacheKey("job1", "build1", "Test A", "boom")
	a2 := s.agenticCacheKey("job1", "build2", "Test A", "boom")
	if a1 == a2 {
		t.Errorf("agentic key should differ between builds: %q vs %q", a1, a2)
	}
	if !strings.HasPrefix(a1, "agentic:capi:job1:build1:") {
		t.Errorf("agentic key shape unexpected: %q", a1)
	}
}

// ---------- Universal mode (use_universal_path) ----------

// TestService_UniversalOn_ToolsUnsupportedSetsUnavailable covers the no-fallback
// invariant of the universal path: a tools-unsupported endpoint must not
// silently regress to the curator pipeline (the user explicitly opted into
// an agentic-only flow). The first failure trips ErrToolsUnsupported and is
// marked unavailable; the second failure short-circuits without making any
// HTTP call because the run-scoped tools-unsupported flag stuck.
func TestService_UniversalOn_ToolsUnsupportedSetsUnavailable(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Only one server push: the agentic 400. The second Analyze() must
	// short-circuit before issuing a request.
	srv.push(400, `{"error":{"message":"function calling not supported"}}`)

	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	s := NewService(client, &stubModule{name: "capi", prompt: "user"}, "sys", nil)
	s.EnableAgentic(AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second}, &fakeFactory{}, registry, enabled, true, true /* universalPath */)

	tc1 := newFailedTC("Test A", "msg-a")
	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc1)
	tc2 := newFailedTC("Test B", "msg-b")
	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc2)

	if tc1.AISummary == nil || !strings.Contains(tc1.AISummary.Summary, "AI analysis unavailable") {
		t.Errorf("first failure: expected unavailable summary, got %+v", tc1.AISummary)
	}
	if tc1.AIAnalysis != nil {
		t.Errorf("first failure: AIAnalysis should NOT be set under universal-mode tools-unsupported, got %+v", tc1.AIAnalysis)
	}
	if tc2.AISummary == nil || !strings.Contains(tc2.AISummary.Summary, "AI analysis unavailable") {
		t.Errorf("second failure: expected unavailable summary (sticky flag), got %+v", tc2.AISummary)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Errorf("server calls = %d, want 1 (second failure must bail before HTTP)", got)
	}
}

// TestService_ShouldReanalyze_FloorTable covers the build-level cache
// invalidation: when a cached agentic-mode analysis records fewer tool calls
// than the current MinToolCalls floor OR fewer GCS bytes than the current
// MinGCSBytes floor, the build cache must NOT be trusted (shouldReanalyze
// returns true) so the loop runs again and the new floor can actually take
// effect. Curator caches are unaffected because they legitimately have
// ToolCalls=0 and GCSBytes=0 by design.
func TestService_ShouldReanalyze_FloorTable(t *testing.T) {
	cases := []struct {
		name         string
		cachedMode   string
		cachedCalls  int
		cachedGCS    int
		desiredMode  string
		minToolCalls int
		minGCSBytes  int
		want         bool
	}{
		{"agentic_below_calls_floor", AgenticMode, 0, 0, AgenticMode, 3, 0, true},
		{"agentic_at_calls_floor", AgenticMode, 3, 0, AgenticMode, 3, 0, false},
		{"agentic_above_calls_floor", AgenticMode, 5, 0, AgenticMode, 3, 0, false},
		{"universal_below_calls_floor", UniversalMode, 0, 0, UniversalMode, 3, 0, true},
		{"universal_at_calls_floor", UniversalMode, 3, 0, UniversalMode, 3, 0, false},
		{"agentic_zero_floors_no_invalidation", AgenticMode, 0, 0, AgenticMode, 0, 0, false},
		{"curator_floors_ignored", curatorMode, 0, 0, curatorMode, 3, 50_000, false},
		{"mode_mismatch_overrides_floors", curatorMode, 5, 200_000, AgenticMode, 0, 0, true},
		{"agentic_below_gcs_floor_only", AgenticMode, 10, 1_000, AgenticMode, 0, 50_000, true},
		{"agentic_at_gcs_floor_only", AgenticMode, 10, 50_000, AgenticMode, 0, 50_000, false},
		{"agentic_above_gcs_floor_only", AgenticMode, 10, 200_000, AgenticMode, 0, 50_000, false},
		{"agentic_meets_calls_misses_gcs", AgenticMode, 5, 10_000, AgenticMode, 5, 50_000, true},
		{"agentic_misses_calls_meets_gcs", AgenticMode, 1, 200_000, AgenticMode, 5, 50_000, true},
		{"agentic_meets_both", AgenticMode, 5, 50_000, AgenticMode, 5, 50_000, false},
		{"universal_below_gcs_floor", UniversalMode, 10, 1_000, UniversalMode, 0, 50_000, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{agenticOpts: AgenticOptions{MinToolCalls: tc.minToolCalls, MinGCSBytes: tc.minGCSBytes}}
			testCase := &models.TestCase{
				AIAnalysis: &models.AIAnalysis{Mode: tc.cachedMode, ToolCalls: tc.cachedCalls, GCSBytes: tc.cachedGCS},
			}
			if got := s.shouldReanalyze(testCase, tc.desiredMode); got != tc.want {
				t.Errorf("shouldReanalyze cached(mode=%q, calls=%d, gcs=%d) desired=%q floors(calls=%d, gcs=%d) = %v, want %v",
					tc.cachedMode, tc.cachedCalls, tc.cachedGCS, tc.desiredMode, tc.minToolCalls, tc.minGCSBytes, got, tc.want)
			}
		})
	}
}

// TestService_BelowFloor_ReanalyzesBuildCacheEntry exercises the full Analyze
// path: a build-cached agentic-universal analysis with ToolCalls below the
// current floor must trigger a fresh API call instead of being served from
// the build cache. Regression for the bug where shouldReanalyze only checked
// Mode and let pre-floor zero-tool entries bypass the floor forever.
func TestService_BelowFloor_ReanalyzesBuildCacheEntry(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Final after one tool call to satisfy floor=1.
	srv.push(200, chatRespToolCall("call_1", "list_artifacts", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespFinal(`{"summary":"fresh post-floor","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"f","relevant_files":[]}`))

	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	s := NewService(client, &stubModule{name: "capi", prompt: "user"}, "sys", nil)
	s.EnableAgentic(
		AgenticOptions{MaxIters: 4, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second, MinToolCalls: 1},
		&fakeFactory{}, registry, enabled, true, true, /* universalPath */
	)

	// Pre-floor cached universal analysis with ToolCalls=0 already attached
	// to the test case (as it would be after loading data/jobs/*.json).
	tc := newFailedTC("Test A", "msg")
	tc.AISummary = &models.AISummary{Summary: "stale zero-tool"}
	tc.AIAnalysis = &models.AIAnalysis{RootCause: "stale", Mode: UniversalMode, ToolCalls: 0}

	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if tc.AIAnalysis.ToolCalls < 1 {
		t.Errorf("ToolCalls = %d, want >= 1 (fresh analysis should satisfy floor)", tc.AIAnalysis.ToolCalls)
	}
	if !strings.Contains(tc.AISummary.Summary, "fresh post-floor") {
		t.Errorf("expected fresh summary, got %q (build-cached pre-floor entry should have been invalidated)", tc.AISummary.Summary)
	}
}

// newServiceTestRegistry returns a filesystem-only registry usable in
// service-level tests. K8s tier is omitted because none of these tests
// drive cluster discovery; agentic loops here exit on the first chat
// response (either ErrToolsUnsupported or a final JSON message).
func newServiceTestRegistry(t *testing.T) (*tools.Registry, []string) {
	t.Helper()
	r := tools.NewRegistry()
	filesystem.Register(r)
	enabled, err := r.Enable([]string{"filesystem"})
	if err != nil {
		t.Fatalf("registry.Enable: %v", err)
	}
	return r, enabled
}

// ---------- Test helper: fake factory ----------

type fakeFactory struct{}

func (f *fakeFactory) ForBuild(_, _ string) artifacts.Browser {
	return &fakeBrowser{}
}

// Ensure stubModule satisfies the Module interface (compile-time check).
var _ Module = (*stubModule)(nil)
var _ AgenticPreferrer = (*stubPreferrer)(nil)
var _ artifacts.Factory = (*fakeFactory)(nil)