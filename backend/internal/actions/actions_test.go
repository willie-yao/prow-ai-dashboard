package actions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediation"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/resolve"
)

// writeJobDetail writes a jobs/<name>.json fixture under dataDir.
func writeJobDetail(t *testing.T, dataDir, name string, detail models.JobDetail) {
	t.Helper()
	dir := filepath.Join(dataDir, "jobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func systemicPattern() models.PatternAnalysis {
	pa := models.PatternAnalysis{JobID: "periodic-x", Systemic: true, SharedRootCause: "etcd timeout"}
	pa.ID = models.PatternID(pa)
	return pa
}

func TestResolve_Unresolve_RoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	pa := models.PatternAnalysis{JobID: "periodic-x", Systemic: true, SharedRootCause: "etcd timeout", SharedBuilds: []string{"100", "250", "175"}}
	pa.ID = models.PatternID(pa)
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pa}})
	s := NewService(&project.Config{}, dataDir, AIConfig{})

	if err := s.Resolve(pa.ID, "willie-yao", "fixed by test-infra #123"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	st := resolve.Load(dataDir)
	e, ok := st.Resolved[pa.ID]
	if !ok {
		t.Fatal("pattern should be resolved")
	}
	if e.ResolvedBy != "willie-yao" || e.Note != "fixed by test-infra #123" {
		t.Errorf("entry metadata wrong: %+v", e)
	}
	if e.Watermark != "250" { // highest of the shared builds
		t.Errorf("watermark = %q, want 250", e.Watermark)
	}

	if err := s.Unresolve(pa.ID); err != nil {
		t.Fatalf("Unresolve: %v", err)
	}
	if resolve.Load(dataDir).IsResolved(pa.ID) {
		t.Fatal("pattern should be unresolved")
	}
}

func TestResolve_NonSystemicRejected(t *testing.T) {
	dataDir := t.TempDir()
	pa := models.PatternAnalysis{JobID: "periodic-x", Systemic: false, SharedRootCause: "flake"}
	pa.ID = models.PatternID(pa)
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pa}})
	s := NewService(&project.Config{}, dataDir, AIConfig{})
	if err := s.Resolve(pa.ID, "willie-yao", ""); err == nil {
		t.Fatal("expected non-systemic resolve to be rejected")
	}
}

func TestUnresolve_NotFound(t *testing.T) {
	s := NewService(&project.Config{}, t.TempDir(), AIConfig{})
	if err := s.Unresolve("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPreviewIssue_NotFound(t *testing.T) {
	dataDir := t.TempDir()
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: "periodic-x"})
	cfg := &project.Config{Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}

	s := NewService(cfg, dataDir, AIConfig{})
	_, err := s.PreviewIssue(context.Background(), "does-not-exist", "tok", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPreviewIssue_NoRepoResolved(t *testing.T) {
	dataDir := t.TempDir()
	pa := systemicPattern()
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pa}})
	// No issues repo and no branding source repo -> unresolved.
	cfg := &project.Config{}

	s := NewService(cfg, dataDir, AIConfig{})
	_, err := s.PreviewIssue(context.Background(), pa.ID, "tok", "")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want repo-resolution error, got %v", err)
	}
}

func TestPreviewIssue_NonSystemicNotActionable(t *testing.T) {
	dataDir := t.TempDir()
	pa := models.PatternAnalysis{JobID: "periodic-x", Systemic: false, SharedRootCause: "flake"}
	pa.ID = models.PatternID(pa)
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pa}})
	cfg := &project.Config{Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}

	s := NewService(cfg, dataDir, AIConfig{})
	_, err := s.PreviewIssue(context.Background(), pa.ID, "tok", "")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want not-actionable error, got %v", err)
	}
}

func TestPreviewFix_AINotConfigured(t *testing.T) {
	dataDir := t.TempDir()
	pa := systemicPattern()
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pa}})
	cfg := &project.Config{AI: &project.AI{FixPRs: &project.FixPRs{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}}

	s := NewService(cfg, dataDir, AIConfig{})
	_, err := s.PreviewFix(context.Background(), pa.ID, "tok", "")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want AI-not-configured error, got %v", err)
	}
}

func TestPreviewCache_TokenOwnershipAndConsumption(t *testing.T) {
	s := NewService(&project.Config{}, t.TempDir(), AIConfig{})
	tok, err := s.stash("owner-token", &previewEntry{kind: "issue"})
	if err != nil {
		t.Fatal(err)
	}

	// A different admin's token must not resolve the draft.
	if _, err := s.take("someone-else", tok); !errors.Is(err, ErrPreviewNotFound) {
		t.Fatalf("cross-admin take: want ErrPreviewNotFound, got %v", err)
	}
	// The owning admin resolves it once...
	if _, err := s.take("owner-token", tok); err != nil {
		t.Fatalf("owner take: %v", err)
	}
	// ...and the token is single-use.
	if _, err := s.take("owner-token", tok); !errors.Is(err, ErrPreviewNotFound) {
		t.Fatalf("reuse take: want ErrPreviewNotFound, got %v", err)
	}
}

func TestPreviewCache_Expiry(t *testing.T) {
	s := NewService(&project.Config{}, t.TempDir(), AIConfig{})
	tok, err := s.stash("owner-token", &previewEntry{kind: "issue"})
	if err != nil {
		t.Fatal(err)
	}
	s.pmu.Lock()
	s.previews[tok].createdAt = time.Now().Add(-previewTTL - time.Minute)
	s.pmu.Unlock()
	if _, err := s.take("owner-token", tok); !errors.Is(err, ErrPreviewNotFound) {
		t.Fatalf("expired take: want ErrPreviewNotFound, got %v", err)
	}
}

func TestSafeReason_RedactsAIErrorsPassesOurs(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"chat returned 401: unauthorized: <provider body>", "the AI service could not complete the request"},
		{"AuthenticateToken authentication failed: unauthorized", "the AI service could not complete the request"},
		{"the model could not produce a code change for this failure", "the model could not produce a code change for this failure"},
		{"no candidate files in the repo matched the failure", "no candidate files in the repo matched the failure"},
		{"", "the fix could not be generated"},
	}
	for _, c := range cases {
		if got := safeReason(c.in); got != c.want {
			t.Errorf("safeReason(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSafeReason_Truncates(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := safeReason(long)
	if len([]rune(got)) > 302 { // 300 + ellipsis
		t.Errorf("safeReason did not truncate: len=%d", len([]rune(got)))
	}
}

func TestRetryContextUsesFailedAttemptEvidence(t *testing.T) {
	service := NewService(&project.Config{}, t.TempDir(), AIConfig{})
	state := remediation.NewState()
	state.Remediations["pattern"] = &remediation.Remediation{
		ID: "pattern", JobID: "job",
		Attempts: []remediation.Attempt{{
			Number: 1, URL: "https://github.com/o/r/pull/7", PatchHash: "patch",
			Status: remediation.StatusStillFailingSameCause, OutcomeReason: "same signature",
			Observations: []remediation.BuildObservation{{BuildID: "12", Outcome: remediation.OutcomeSameCause}},
		}},
	}
	if err := state.Save(service.dataDir); err != nil {
		t.Fatal(err)
	}
	retry, patchHash, instruction, err := service.retryContext("pattern")
	if err != nil {
		t.Fatal(err)
	}
	if !retry || patchHash != "patch" || !strings.Contains(instruction, "pull/7") || !strings.Contains(instruction, "12") {
		t.Fatalf("retry=%v hash=%q instruction=%q", retry, patchHash, instruction)
	}
}

func TestRetryContextEnforcesAttemptCap(t *testing.T) {
	service := NewService(&project.Config{}, t.TempDir(), AIConfig{})
	state := remediation.NewState()
	state.Remediations["pattern"] = &remediation.Remediation{ID: "pattern", JobID: "job", Attempts: []remediation.Attempt{
		{Number: 1}, {Number: 2, Status: remediation.StatusStillFailingSameCause},
	}}
	if err := state.Save(service.dataDir); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := service.retryContext("pattern"); err == nil {
		t.Fatal("expected retry limit error")
	}
}

func TestRetryContextDoesNotMatchUnrelatedFindingOnSameJob(t *testing.T) {
	service := NewService(&project.Config{}, t.TempDir(), AIConfig{})
	state := remediation.NewState()
	state.Remediations["old"] = &remediation.Remediation{
		ID: "old", FindingID: "old", JobID: "job",
		Attempts: []remediation.Attempt{{Status: remediation.StatusStillFailingSameCause}},
	}
	if err := state.Save(service.dataDir); err != nil {
		t.Fatal(err)
	}
	retry, _, _, err := service.retryContext("new")
	if err != nil {
		t.Fatal(err)
	}
	if retry {
		t.Fatal("unrelated finding was treated as a retry")
	}
}

func TestRetryReservationReturnsCompletedResult(t *testing.T) {
	service := NewService(&project.Config{}, t.TempDir(), AIConfig{})
	state := remediation.NewState()
	state.Remediations["pattern"] = &remediation.Remediation{
		ID: "pattern", FindingID: "pattern",
		Attempts: []remediation.Attempt{{Number: 1, Status: remediation.StatusStillFailingSameCause}},
	}
	if err := state.Save(service.dataDir); err != nil {
		t.Fatal(err)
	}
	existing, reservationID, err := service.reserveRetry("pattern", "patch")
	if err != nil || existing != "" || reservationID == "" {
		t.Fatalf("existing=%q reservation=%q err=%v", existing, reservationID, err)
	}
	if _, _, err := service.reserveRetry("pattern", "patch"); err == nil {
		t.Fatal("expected in-progress reservation error")
	}
	if err := service.completeRetryReservation("pattern", reservationID, "https://github.com/o/r/pull/8"); err != nil {
		t.Fatal(err)
	}
	existing, _, err = service.reserveRetry("pattern", "patch")
	if err != nil || existing != "https://github.com/o/r/pull/8" {
		t.Fatalf("existing=%q err=%v", existing, err)
	}
}
