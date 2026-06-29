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

// stubModule satisfies ai.Module for service tests. The prompt is returned
// verbatim by AnalysisPrompt.
type stubModule struct {
	name   string
	prompt string
}

func (m *stubModule) Name() string { return m.name }
func (m *stubModule) AnalysisPrompt(_ context.Context, _ *http.Client, _ *models.BuildResult, _ *models.TestCase, _ int) string {
	return m.prompt
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

func TestService_Agentic_TagsModeAgentic(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	final := `{"summary":"x","is_transient":false,"root_cause":"y","severity":"Low","suggested_fix":"f","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	s := NewService(client, &stubModule{name: "kubernetes", prompt: "user"}, "sys", nil)
	s.EnableAgentic(AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}, &fakeFactory{}, registry, enabled)

	tc := newFailedTC("Test A", "failure msg")
	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if tc.AIAnalysis == nil {
		t.Fatal("AIAnalysis nil")
	}
	if tc.AIAnalysis.Mode != AgenticMode {
		t.Errorf("Mode = %q, want %q", tc.AIAnalysis.Mode, AgenticMode)
	}
}

func TestService_ReanalyzeOnModeChange(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Agentic call returns final JSON; a cached entry with a foreign mode
	// should be invalidated.
	final := `{"summary":"new agentic","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"f","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	s := NewService(client, &stubModule{name: "kubernetes", prompt: "user"}, "sys", nil)
	s.EnableAgentic(AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}, &fakeFactory{}, registry, enabled)

	// Test case already has a cached analysis with a foreign mode.
	tc := newFailedTC("Test A", "msg")
	tc.AISummary = &models.AISummary{Summary: "stale summary"}
	tc.AIAnalysis = &models.AIAnalysis{RootCause: "stale root cause", Mode: "old-mode"}

	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if tc.AIAnalysis.Mode != AgenticMode {
		t.Errorf("Mode = %q, want %q (stale non-agentic entry should be re-analyzed)", tc.AIAnalysis.Mode, AgenticMode)
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
	registry, enabled := newServiceTestRegistry(t)
	s := NewService(client, &stubModule{name: "kubernetes", prompt: "user"}, "sys", nil)
	s.EnableAgentic(AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}, &fakeFactory{}, registry, enabled)

	tc := newFailedTC("Test A", "msg")
	tc.AISummary = &models.AISummary{Summary: "cached"}
	tc.AIAnalysis = &models.AIAnalysis{RootCause: "cached", Mode: AgenticMode, PromptHash: PromptFingerprint("sys")}

	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if got := atomic.LoadInt32(&srv.calls); got != 0 {
		t.Errorf("server calls = %d, want 0 (existing agentic analysis should be kept)", got)
	}
	if tc.AIAnalysis.RootCause != "cached" {
		t.Errorf("expected cached root cause to be preserved, got %q", tc.AIAnalysis.RootCause)
	}
}

func TestService_CacheKeyShape(t *testing.T) {
	s := &Service{module: &stubModule{name: "kubernetes"}}
	// Agentic key encodes job + build so two builds of the same test never collide.
	a1 := s.agenticCacheKey("job1", "build1", "Test A", "boom")
	a2 := s.agenticCacheKey("job1", "build2", "Test A", "boom")
	if a1 == a2 {
		t.Errorf("agentic key should differ between builds: %q vs %q", a1, a2)
	}
	if !strings.HasPrefix(a1, "agentic:kubernetes:job1:build1:") {
		t.Errorf("agentic key shape unexpected: %q", a1)
	}
}

// TestService_ShouldReanalyze_PromptHash verifies prompt changes invalidate
// cached analysis while matching prompts are reused.
func TestService_ShouldReanalyze_PromptHash(t *testing.T) {
	s := &Service{systemPrompt: "engine base + my prompt"}
	// Meets the (zero) floors and is agentic mode, so only the prompt gate
	// can force re-analysis here.
	mk := func(promptHash string) *models.TestCase {
		return &models.TestCase{AIAnalysis: &models.AIAnalysis{Mode: AgenticMode, PromptHash: promptHash}}
	}

	if s.shouldReanalyze(mk(PromptFingerprint("engine base + my prompt"))) {
		t.Error("matching prompt hash should be reused, got re-analyze")
	}
	if !s.shouldReanalyze(mk(PromptFingerprint("engine base + an OLD prompt"))) {
		t.Error("changed prompt hash should force re-analysis")
	}
	// Unstamped entries re-analyze once.
	if !s.shouldReanalyze(mk("")) {
		t.Error("unstamped (pre-feature) entry should re-analyze once")
	}
}

// ---------- tools-unsupported (no fallback) ----------

// TestService_ToolsUnsupported_SetsUnavailable verifies tools-unsupported
// endpoints mark failures unavailable and short-circuit subsequent failures.
func TestService_ToolsUnsupported_SetsUnavailable(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Only one server push: the second Analyze short-circuits before HTTP.
	srv.push(400, `{"error":{"message":"function calling not supported"}}`)

	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	s := NewService(client, &stubModule{name: "kubernetes", prompt: "user"}, "sys", nil)
	s.EnableAgentic(AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}, &fakeFactory{}, registry, enabled)

	tc1 := newFailedTC("Test A", "msg-a")
	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc1)
	tc2 := newFailedTC("Test B", "msg-b")
	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc2)

	if tc1.AISummary == nil || !strings.Contains(tc1.AISummary.Summary, "AI analysis unavailable") {
		t.Errorf("first failure: expected unavailable summary, got %+v", tc1.AISummary)
	}
	if tc1.AIAnalysis != nil {
		t.Errorf("first failure: AIAnalysis should NOT be set under tools-unsupported, got %+v", tc1.AIAnalysis)
	}
	if tc2.AISummary == nil || !strings.Contains(tc2.AISummary.Summary, "AI analysis unavailable") {
		t.Errorf("second failure: expected unavailable summary (sticky flag), got %+v", tc2.AISummary)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Errorf("server calls = %d, want 1 (second failure must bail before HTTP)", got)
	}
}

// TestService_ShouldReanalyze_FloorTable covers cache invalidation for mode
// mismatches and agentic floor changes.
func TestService_ShouldReanalyze_FloorTable(t *testing.T) {
	cases := []struct {
		name         string
		cachedMode   string
		cachedCalls  int
		cachedGCS    int
		minToolCalls int
		minGCSBytes  int
		want         bool
	}{
		{"agentic_below_calls_floor", AgenticMode, 0, 0, 3, 0, true},
		{"agentic_at_calls_floor", AgenticMode, 3, 0, 3, 0, false},
		{"agentic_above_calls_floor", AgenticMode, 5, 0, 3, 0, false},
		{"agentic_zero_floors_no_invalidation", AgenticMode, 0, 0, 0, 0, false},
		{"stale_mode_always_reanalyzes", "old-mode", 5, 200_000, 0, 0, true},
		{"empty_mode_always_reanalyzes", "", 5, 200_000, 0, 0, true},
		{"agentic_below_gcs_floor_only", AgenticMode, 10, 1_000, 0, 50_000, true},
		{"agentic_at_gcs_floor_only", AgenticMode, 10, 50_000, 0, 50_000, false},
		{"agentic_above_gcs_floor_only", AgenticMode, 10, 200_000, 0, 50_000, false},
		{"agentic_meets_calls_misses_gcs", AgenticMode, 5, 10_000, 5, 50_000, true},
		{"agentic_misses_calls_meets_gcs", AgenticMode, 1, 200_000, 5, 50_000, true},
		{"agentic_meets_both", AgenticMode, 5, 50_000, 5, 50_000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{systemPrompt: "sys", agenticOpts: AgenticOptions{MinToolCalls: tc.minToolCalls, MinGCSBytes: tc.minGCSBytes}}
			testCase := &models.TestCase{
				AIAnalysis: &models.AIAnalysis{Mode: tc.cachedMode, ToolCalls: tc.cachedCalls, GCSBytes: tc.cachedGCS, PromptHash: PromptFingerprint("sys")},
			}
			if got := s.shouldReanalyze(testCase); got != tc.want {
				t.Errorf("shouldReanalyze cached(mode=%q, calls=%d, gcs=%d) floors(calls=%d, gcs=%d) = %v, want %v",
					tc.cachedMode, tc.cachedCalls, tc.cachedGCS, tc.minToolCalls, tc.minGCSBytes, got, tc.want)
			}
		})
	}
}

// TestService_BelowFloor_ReanalyzesBuildCacheEntry exercises the full Analyze
// path: a build-cached agentic analysis with ToolCalls below the current floor
// must trigger a fresh API call instead of being served from the build cache.
func TestService_BelowFloor_ReanalyzesBuildCacheEntry(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Final after one tool call to satisfy floor=1.
	srv.push(200, chatRespToolCall("call_1", "list_artifacts", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespFinal(`{"summary":"fresh post-floor","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"f","relevant_files":[]}`))

	client := newAgenticTestClient(t, srv.URL)
	registry, enabled := newServiceTestRegistry(t)
	s := NewService(client, &stubModule{name: "kubernetes", prompt: "user"}, "sys", nil)
	s.EnableAgentic(
		AgenticOptions{MaxIters: 4, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second, MinToolCalls: 1},
		&fakeFactory{}, registry, enabled,
	)

	// Pre-floor cached agentic analysis with ToolCalls=0 is already attached.
	tc := newFailedTC("Test A", "msg")
	tc.AISummary = &models.AISummary{Summary: "stale zero-tool"}
	tc.AIAnalysis = &models.AIAnalysis{RootCause: "stale", Mode: AgenticMode, ToolCalls: 0}

	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if tc.AIAnalysis.ToolCalls < 1 {
		t.Errorf("ToolCalls = %d, want >= 1 (fresh analysis should satisfy floor)", tc.AIAnalysis.ToolCalls)
	}
	if !strings.Contains(tc.AISummary.Summary, "fresh post-floor") {
		t.Errorf("expected fresh summary, got %q (build-cached pre-floor entry should have been invalidated)", tc.AISummary.Summary)
	}
}

// newServiceTestRegistry returns a filesystem-only registry for service tests.
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

// fileFactory hands out a browser backed by a fixed file set so cascade tests
// can ground a triage run with real reads.
type fileFactory struct{ files map[string][]byte }

func (f *fileFactory) ForBuild(_, _ string) artifacts.Browser {
	return &fakeBrowser{files: f.files}
}

// Compile-time interface checks.
var _ Module = (*stubModule)(nil)
var _ artifacts.Factory = (*fakeFactory)(nil)
var _ artifacts.Factory = (*fileFactory)(nil)

// ---------- Model cascade (triage tier) ----------

func TestTriageAccepts(t *testing.T) {
	s := &Service{triageOpts: AgenticOptions{MinToolCalls: 1, MinGCSBytes: 100}}
	cases := []struct {
		name            string
		transient       bool
		budgetExhausted bool
		toolCalls       int
		gcsBytes        int
		want            bool
	}{
		{"grounded transient", true, false, 2, 200, true},
		{"real bug", false, false, 2, 200, false},
		{"below tool floor", true, false, 0, 200, false},
		{"below gcs floor", true, false, 2, 50, false},
		{"budget exhausted", true, true, 2, 200, false},
	}
	for _, c := range cases {
		sum := &models.AISummary{IsTransient: c.transient}
		an := &models.AIAnalysis{BudgetExhausted: c.budgetExhausted, ToolCalls: c.toolCalls, GCSBytes: c.gcsBytes}
		if got := s.triageAccepts(sum, an); got != c.want {
			t.Errorf("%s: triageAccepts = %v, want %v", c.name, got, c.want)
		}
	}
	if s.triageAccepts(nil, nil) {
		t.Error("nil summary/analysis must not be accepted")
	}
}

// newCascadeService wires a Service with a deep client and a triage client
// pointing at separate scripted servers, grounded by one build-log.txt file.
func newCascadeService(t *testing.T, deepURL, triageURL string, triageOpts AgenticOptions) *Service {
	t.Helper()
	registry, enabled := newServiceTestRegistry(t)
	factory := &fileFactory{files: map[string][]byte{"build-log.txt": []byte(strings.Repeat("log line\n", 30))}}
	s := NewService(newAgenticTestClient(t, deepURL), &stubModule{name: "kubernetes", prompt: "user"}, "sys", nil)
	s.EnableAgentic(AgenticOptions{MaxIters: 5, ModelByteBudget: 1_000_000, GCSByteBudget: 1_000_000, Timeout: 30 * time.Second}, factory, registry, enabled)
	s.EnableTriage(newAgenticTestClient(t, triageURL), triageOpts)
	return s
}

func TestService_Cascade_GroundedTransientResolvedByTriage(t *testing.T) {
	shrinkCallDelay(t)
	triageSrv := newScriptedChatServer(t)
	triageSrv.push(200, chatRespToolCall("c1", "read_artifact", map[string]interface{}{"path": "build-log.txt"}))
	triageSrv.push(200, chatRespFinal(`{"summary":"flake","is_transient":true,"root_cause":"network blip that recovered","severity":"Transient-Ignore","suggested_fix":"retry","relevant_files":["build-log.txt"]}`))
	deepSrv := newScriptedChatServer(t) // must not be called

	s := newCascadeService(t, deepSrv.URL, triageSrv.URL, AgenticOptions{MaxIters: 5, ModelByteBudget: 1_000_000, GCSByteBudget: 1_000_000, Timeout: 30 * time.Second, MinToolCalls: 1, MinGCSBytes: 1})
	tc := newFailedTC("Test A", "boom")
	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if tc.AISummary == nil || !tc.AISummary.IsTransient {
		t.Fatalf("want transient summary, got %+v", tc.AISummary)
	}
	if tc.AIAnalysis == nil || tc.AIAnalysis.Tier != "triage" {
		t.Fatalf("Tier = %q, want triage", tierOf(tc))
	}
	if tc.AIAnalysis.Escalated {
		t.Error("Escalated = true, want false for a triage-resolved failure")
	}
	if got := atomic.LoadInt32(&deepSrv.calls); got != 0 {
		t.Errorf("deep tier called %d times, want 0", got)
	}
	if r, e := s.TriageStats(); r != 1 || e != 0 {
		t.Errorf("TriageStats = (%d,%d), want (1,0)", r, e)
	}
}

func TestService_Cascade_RealBugEscalatesToDeep(t *testing.T) {
	shrinkCallDelay(t)
	triageSrv := newScriptedChatServer(t)
	triageSrv.push(200, chatRespToolCall("c1", "read_artifact", map[string]interface{}{"path": "build-log.txt"}))
	triageSrv.push(200, chatRespFinal(`{"summary":"real","is_transient":false,"root_cause":"nil deref in reconciler","severity":"High","suggested_fix":"guard the nil","relevant_files":["build-log.txt"]}`))
	deepSrv := newScriptedChatServer(t)
	deepSrv.push(200, chatRespFinal(`{"summary":"deep","is_transient":false,"root_cause":"nil deref in reconciler at line 42","severity":"High","suggested_fix":"guard p before deref","relevant_files":["build-log.txt"]}`))

	s := newCascadeService(t, deepSrv.URL, triageSrv.URL, AgenticOptions{MaxIters: 5, ModelByteBudget: 1_000_000, GCSByteBudget: 1_000_000, Timeout: 30 * time.Second, MinToolCalls: 1, MinGCSBytes: 1})
	tc := newFailedTC("Test B", "boom")
	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if tc.AIAnalysis == nil || tc.AIAnalysis.Tier != "deep" {
		t.Fatalf("Tier = %q, want deep", tierOf(tc))
	}
	if !tc.AIAnalysis.Escalated {
		t.Error("Escalated = false, want true after escalation")
	}
	if got := atomic.LoadInt32(&deepSrv.calls); got == 0 {
		t.Error("deep tier was not called on escalation")
	}
	if r, e := s.TriageStats(); r != 0 || e != 1 {
		t.Errorf("TriageStats = (%d,%d), want (0,1)", r, e)
	}
}

func TestService_Cascade_TriageErrorEscalatesToDeep(t *testing.T) {
	shrinkCallDelay(t)
	triageSrv := newScriptedChatServer(t)
	triageSrv.push(500, "triage boom")
	deepSrv := newScriptedChatServer(t)
	deepSrv.push(200, chatRespFinal(`{"summary":"deep","is_transient":false,"root_cause":"real","severity":"High","suggested_fix":"fix it","relevant_files":[]}`))

	s := newCascadeService(t, deepSrv.URL, triageSrv.URL, AgenticOptions{MaxIters: 5, ModelByteBudget: 1_000_000, GCSByteBudget: 1_000_000, Timeout: 30 * time.Second})
	tc := newFailedTC("Test C", "boom")
	s.Analyze(context.Background(), &http.Client{}, "j", "logs/j/1/", newRun("j", "1"), tc)

	if tc.AIAnalysis == nil || tc.AIAnalysis.Tier != "deep" {
		t.Fatalf("Tier = %q, want deep after a triage infra error", tierOf(tc))
	}
	if got := atomic.LoadInt32(&deepSrv.calls); got == 0 {
		t.Error("deep tier was not called after triage error")
	}
}

func tierOf(tc *models.TestCase) string {
	if tc.AIAnalysis == nil {
		return "<nil analysis>"
	}
	return tc.AIAnalysis.Tier
}

func TestService_TriageResult_RevalidatedAgainstTriageFloors(t *testing.T) {
	// A cached triage-tier result is checked against the triage floors, not the
	// deep floors, so raising the triage floor forces re-analysis.
	s := &Service{
		systemPrompt: "sys",
		agenticOpts:  AgenticOptions{MinToolCalls: 0, MinGCSBytes: 0},
		triageClient: &Client{},
		triageOpts:   AgenticOptions{MinToolCalls: 3, MinGCSBytes: 200_000},
	}
	tc := &models.TestCase{AIAnalysis: &models.AIAnalysis{
		Mode: AgenticMode, Tier: "triage", ToolCalls: 1, GCSBytes: 1000,
		PromptHash: PromptFingerprint("sys"),
	}}
	if !s.belowCurrentAgenticFloor(tc) {
		t.Error("triage result below the triage floor should re-analyze")
	}
	// Meets the triage floor: no re-analysis.
	tc.AIAnalysis.ToolCalls = 5
	tc.AIAnalysis.GCSBytes = 300_000
	if s.belowCurrentAgenticFloor(tc) {
		t.Error("triage result meeting the triage floor should not re-analyze")
	}
	// Cascade disabled (triageClient nil): a stale triage result must re-analyze.
	s.triageClient = nil
	if !s.belowCurrentAgenticFloor(tc) {
		t.Error("triage result should re-analyze once the cascade is disabled")
	}
}

// ---------- setUnavailable retry semantics ----------

// TestSetUnavailable_RetrySemantics verifies unavailable summaries are replaced
// on retry while transient and real summaries are preserved.
func TestSetUnavailable_RetrySemantics(t *testing.T) {
	s := &Service{}

	t.Run("sets when nil", func(t *testing.T) {
		tc := newFailedTC("t", "m")
		s.setUnavailable(tc, errEndpointA)
		if tc.AISummary == nil || !strings.Contains(tc.AISummary.Summary, "endpoint A") {
			t.Fatalf("want endpoint A summary, got %+v", tc.AISummary)
		}
	})

	t.Run("overwrites a prior unavailable summary", func(t *testing.T) {
		tc := newFailedTC("t", "m")
		tc.AISummary = &models.AISummary{
			GeneratedAt: "2000-01-01T00:00:00Z",
			Summary:     unavailablePrefix + "endpoint A is down",
		}
		s.setUnavailable(tc, errEndpointB)
		if !strings.Contains(tc.AISummary.Summary, "endpoint B") {
			t.Fatalf("stale error not replaced: %q", tc.AISummary.Summary)
		}
		if tc.AISummary.GeneratedAt == "2000-01-01T00:00:00Z" {
			t.Error("timestamp not refreshed on retry")
		}
	})

	t.Run("preserves a transient classification", func(t *testing.T) {
		tc := newFailedTC("t", "m")
		tc.AISummary = &models.AISummary{Summary: "infra flake", IsTransient: true}
		s.setUnavailable(tc, errEndpointB)
		if !tc.AISummary.IsTransient || tc.AISummary.Summary != "infra flake" {
			t.Fatalf("transient summary clobbered: %+v", tc.AISummary)
		}
	})

	t.Run("preserves a real summary", func(t *testing.T) {
		tc := newFailedTC("t", "m")
		tc.AISummary = &models.AISummary{Summary: "real root cause"}
		s.setUnavailable(tc, errEndpointB)
		if tc.AISummary.Summary != "real root cause" {
			t.Fatalf("real summary clobbered: %q", tc.AISummary.Summary)
		}
	})

	t.Run("preserves a real summary even with the unavailable prefix", func(t *testing.T) {
		// A model result is identified by an attached AIAnalysis, not just by
		// its text, so a summary that happens to start with the prefix is not
		// mistaken for an engine placeholder.
		tc := newFailedTC("t", "m")
		tc.AISummary = &models.AISummary{Summary: unavailablePrefix + "is part of the real analysis"}
		tc.AIAnalysis = &models.AIAnalysis{Mode: AgenticMode}
		s.setUnavailable(tc, errEndpointB)
		if !strings.Contains(tc.AISummary.Summary, "real analysis") {
			t.Fatalf("real summary with prefix clobbered: %q", tc.AISummary.Summary)
		}
	})
}

var (
	errEndpointA = fmtErr("endpoint A")
	errEndpointB = fmtErr("endpoint B")
)

func fmtErr(s string) error { return &simpleErr{s} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }
