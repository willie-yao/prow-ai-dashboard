package eval

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aitest"
)

// fixtureRoot reuses the e2e bucket fixture as a dataset artifact root.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "e2e", "testdata", "bucket"))
	if err != nil {
		t.Fatalf("abs fixture: %v", err)
	}
	return abs
}

func newScriptedRunner(t *testing.T, script *aitest.ScriptServer) *Runner {
	t.Helper()
	r, err := NewRunner(Config{
		ArtifactRoot: fixtureRoot(t),
		SystemPrompt: "system prompt",
		Connect:      ai.Options{Endpoint: script.URL, Model: "eval-model"},
		Opts:         ai.AgenticOptions{MaxIters: 5, ModelByteBudget: 1_000_000, GCSByteBudget: 1_000_000, Timeout: 30 * time.Second},
		EnabledTools: []string{"filesystem"},
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

func exampleCase() Case {
	return Case{
		Name:           "control-plane-timeout",
		Job:            "periodic-example-e2e-main",
		Build:          "101",
		BuildPrefix:    "logs/periodic-example-e2e-main/101/",
		TestName:       "[It] Creates a HA cluster with 3 control plane nodes",
		FailureMessage: "Timed out waiting for 3 control plane machines to exist",
		Labels: Labels{
			IsTransient:       false,
			RootCauseKeywords: []string{"control plane", "timeout"},
			ExpectedFiles:     []string{"build-log.txt"},
		},
	}
}

// TestRunner_ScoreCase runs a real Service.Analyze over the fixture artifacts
// with a scripted model and checks the case is scored end to end: a grounded,
// correct (real-bug) verdict citing a real artifact and matching keywords.
func TestRunner_ScoreCase(t *testing.T) {
	script := aitest.NewScriptServer(t)
	script.PushToolCall("c1", "read_artifact", map[string]any{"path": "build-log.txt"})
	script.PushFinal(`{"summary":"control plane timeout","is_transient":false,` +
		`"root_cause":"Only 2 of 3 control plane machines registered before the timeout",` +
		`"severity":"High","suggested_fix":"investigate the third machine","relevant_files":["build-log.txt"]}`)

	r := newScriptedRunner(t, script)
	s, err := r.ScoreCase(context.Background(), exampleCase())
	if err != nil {
		t.Fatalf("ScoreCase: %v", err)
	}
	if !s.Available {
		t.Error("want available; the scripted analysis produced a result")
	}
	if s.TransientPredicted {
		t.Error("predicted transient, want real bug")
	}
	if !s.Grounded {
		t.Errorf("want grounded; tool_calls=%d gcs_bytes=%d", s.ToolCalls, s.GCSBytes)
	}
	if s.CitationValidity != 1 {
		t.Errorf("citation validity = %v, want 1 (build-log.txt is real)", s.CitationValidity)
	}
	if s.ExpectedFileRecall != 1 {
		t.Errorf("expected-file recall = %v, want 1 (build-log.txt cited)", s.ExpectedFileRecall)
	}
	if s.KeywordRecall != 1 {
		t.Errorf("keyword recall = %v, want 1 (control plane + timeout present)", s.KeywordRecall)
	}
}

// TestRunner_RunDataset aggregates two cases and exercises the scorecard.
func TestRunner_RunDataset(t *testing.T) {
	script := aitest.NewScriptServer(t)
	// The correct (real-bug) case.
	script.PushToolCall("c1", "read_artifact", map[string]any{"path": "build-log.txt"})
	script.PushFinal(`{"summary":"x","is_transient":false,"root_cause":"control plane timeout","severity":"High","suggested_fix":"f","relevant_files":["build-log.txt"]}`)
	// A second case the model wrongly calls transient (a false negative).
	script.PushToolCall("c2", "read_artifact", map[string]any{"path": "build-log.txt"})
	script.PushFinal(`{"summary":"y","is_transient":true,"root_cause":"looks flaky","severity":"Transient-Ignore","suggested_fix":"retry","relevant_files":["build-log.txt"]}`)

	r := newScriptedRunner(t, script)
	c1 := exampleCase()
	c2 := exampleCase()
	c2.Name = "second"
	c2.Labels.RootCauseKeywords = nil
	sc, err := r.RunDataset(context.Background(), []Case{c1, c2})
	if err != nil {
		t.Fatalf("RunDataset: %v", err)
	}
	if sc.Cases != 2 {
		t.Fatalf("cases = %d", sc.Cases)
	}
	if sc.Available != 2 || sc.Coverage != 1 {
		t.Errorf("available=%d coverage=%v, want 2/1 (both produced a result)", sc.Available, sc.Coverage)
	}
	// One correct real-bug (TP), one false negative (FN) -> recall 0.5.
	if sc.TP != 1 || sc.FN != 1 {
		t.Errorf("confusion TP=%d FN=%d, want 1/1", sc.TP, sc.FN)
	}
	if sc.GroundingRate != 1 {
		t.Errorf("grounding rate = %v, want 1", sc.GroundingRate)
	}
}
