package universal

import (
	"context"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// TestAnalysisPrompt_CapsHugeFailureMessage verifies that a very large junit
// failure message (e.g. an AKS KubeRay dump) is clamped before it is embedded
// in the prompt, so it can't overflow the model's context window on the first
// request. Regression test for the iter-1 400 "prompt token count exceeds the
// limit" failures.
func TestAnalysisPrompt_CapsHugeFailureMessage(t *testing.T) {
	huge := "HEAD-MARKER" + strings.Repeat("x", 2*1024*1024) + "TAIL-MARKER"
	tc := &models.TestCase{Name: "[It] KubeRay RayJob", FailureMessage: huge}

	got := (&Module{}).AnalysisPrompt(context.Background(), nil, &models.BuildResult{}, tc, 1)

	// The message cap is 16KB; the surrounding template adds a little more. The
	// input is 2MB+, so a clamped prompt is comfortably under 20KB.
	if len(got) > 20*1024 {
		t.Fatalf("prompt not clamped: len=%d", len(got))
	}
	if strings.Contains(got, huge) {
		t.Errorf("prompt embeds the full uncapped failure message")
	}
	if !strings.Contains(got, "HEAD-MARKER") {
		t.Errorf("clamped message dropped the head (assertion)")
	}
	if !strings.Contains(got, "TAIL-MARKER") {
		t.Errorf("clamped message dropped the tail (summary)")
	}
	if !strings.Contains(got, "bytes elided") {
		t.Errorf("clamped message missing the elision marker")
	}
}

// TestAnalysisPrompt_KeepsSmallFailureMessage confirms a normal-sized message
// is embedded verbatim (no elision).
func TestAnalysisPrompt_KeepsSmallFailureMessage(t *testing.T) {
	msg := "Expected RayJob to complete but it timed out after 10m"
	tc := &models.TestCase{Name: "[It] KubeRay RayJob", FailureMessage: msg}

	got := (&Module{}).AnalysisPrompt(context.Background(), nil, &models.BuildResult{}, tc, 1)

	if !strings.Contains(got, msg) {
		t.Errorf("small failure message should be embedded verbatim")
	}
	if strings.Contains(got, "bytes elided") {
		t.Errorf("small failure message should not be elided")
	}
}

// TestClampHeadTail covers the helper directly.
func TestClampHeadTail(t *testing.T) {
	if got := clampHeadTail("short", 100); got != "short" {
		t.Errorf("under-budget string altered: %q", got)
	}
	if got := clampHeadTail("anything", 0); got != "anything" {
		t.Errorf("non-positive max should return input unchanged: %q", got)
	}
	// head = 30 'H', tail = 10 'T' (H/T do not appear in the elision marker).
	big := strings.Repeat("H", 50) + strings.Repeat("T", 50)
	got := clampHeadTail(big, 40)
	if got == big || !strings.Contains(got, "bytes elided") {
		t.Fatalf("over-budget string not clamped: %q", got)
	}
	if c := strings.Count(got, "H"); c != 30 {
		t.Errorf("head: got %d 'H's, want 30", c)
	}
	if c := strings.Count(got, "T"); c != 10 {
		t.Errorf("tail: got %d 'T's, want 10", c)
	}
	if !strings.HasPrefix(got, strings.Repeat("H", 30)) {
		t.Errorf("clamped output should start with the head: %q", got)
	}
	if !strings.HasSuffix(got, strings.Repeat("T", 10)) {
		t.Errorf("clamped output should end with the tail: %q", got)
	}
}
