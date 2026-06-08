package ai

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunBuildLogTriage_ReturnsSummaryFromBuildLog(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal("Top-level error: etcd learner promotion failed. Investigate the control-plane machine's cloud-init log."))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{files: map[string][]byte{
		"build-log.txt": []byte("line1\nFAIL: etcdserver: can only promote a learner member\nginkgo summary\n"),
	}}

	got := client.runBuildLogTriage(context.Background(), browser)
	if !strings.Contains(got, "etcd learner promotion") {
		t.Errorf("triage summary = %q, want the model's distilled error", got)
	}
	if c := atomic.LoadInt32(&srv.calls); c != 1 {
		t.Errorf("expected exactly 1 triage LLM call, got %d", c)
	}
}

func TestRunBuildLogTriage_NoBuildLogIsNoop(t *testing.T) {
	srv := newScriptedChatServer(t)
	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{files: map[string][]byte{
		"other.txt": []byte("nothing useful"),
	}}

	got := client.runBuildLogTriage(context.Background(), browser)
	if got != "" {
		t.Errorf("missing build-log should yield empty triage, got %q", got)
	}
	if c := atomic.LoadInt32(&srv.calls); c != 0 {
		t.Errorf("no build-log should mean no LLM call, got %d calls", c)
	}
}

func TestRunBuildLogTriage_NilBrowserIsNoop(t *testing.T) {
	srv := newScriptedChatServer(t)
	client := newAgenticTestClient(t, srv.URL)
	if got := client.runBuildLogTriage(context.Background(), nil); got != "" {
		t.Errorf("nil browser should yield empty triage, got %q", got)
	}
}

func TestRunBuildLogTriage_CallFailureIsNoop(t *testing.T) {
	srv := newScriptedChatServer(t)
	srv.push(500, `{"error":"boom"}`)
	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{files: map[string][]byte{
		"build-log.txt": []byte("FAIL: something\n"),
	}}
	if got := client.runBuildLogTriage(context.Background(), browser); got != "" {
		t.Errorf("triage call failure should degrade to empty, got %q", got)
	}
}

func TestWithTriageSeed(t *testing.T) {
	base := "Test failure to investigate.\nTest name: X"
	out := withTriageSeed(base, "Top error: foo")
	if !strings.Contains(out, "Build-log triage") {
		t.Errorf("seed should carry the triage header: %q", out)
	}
	if !strings.Contains(out, "Top error: foo") || !strings.Contains(out, base) {
		t.Errorf("seed should contain both the triage summary and the original prompt")
	}
	// Empty triage leaves the prompt untouched.
	if got := withTriageSeed(base, "   "); got != base {
		t.Errorf("empty triage must not modify the prompt, got %q", got)
	}
}

// TestDoAnalyzeAgentic_BuildLogTriageRunsFirst confirms that with
// BuildLogTriage enabled, the loop makes the triage call before the main
// investigation and still produces a normal analysis.
func TestDoAnalyzeAgentic_BuildLogTriageRunsFirst(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Call 1: triage pre-pass response.
	srv.push(200, chatRespFinal("Top-level error: image pull failed on the worker node."))
	// Call 2: main loop returns final JSON directly.
	final := `{"summary":"image pull failure","is_transient":false,"root_cause":"registry auth expired","severity":"High","suggested_fix":"rotate the pull secret","relevant_files":["build-log.txt"]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{
		files: map[string][]byte{"build-log.txt": []byte("ErrImagePull\nFailed to pull image\n")},
	}
	opts := AgenticOptions{MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second, BuildLogTriage: true}

	summary, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:job:9:zzz", "system", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	// Two calls: one triage, one main loop.
	if c := atomic.LoadInt32(&srv.calls); c != 2 {
		t.Errorf("call count = %d, want 2 (triage + final)", c)
	}
	if summary.Summary != "image pull failure" || analysis.RootCause != "registry auth expired" {
		t.Errorf("unexpected analysis: summary=%q root_cause=%q", summary.Summary, analysis.RootCause)
	}
}

// TestDoAnalyzeAgentic_TriageOffByDefault confirms the pre-pass does not run
// when BuildLogTriage is unset.
func TestDoAnalyzeAgentic_TriageOffByDefault(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	final := `{"summary":"x","is_transient":false,"root_cause":"y","severity":"Low","suggested_fix":"z","relevant_files":["build-log.txt"]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte("FAIL\n")}}
	opts := AgenticOptions{MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second}

	_, _, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:job:9:off", "system", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if c := atomic.LoadInt32(&srv.calls); c != 1 {
		t.Errorf("call count = %d, want 1 (no triage pre-pass)", c)
	}
}
