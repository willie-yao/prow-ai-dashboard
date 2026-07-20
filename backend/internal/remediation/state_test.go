package remediation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	state := NewState()
	state.Remediations["x"] = &Remediation{ID: "x", FindingID: "finding", Attempts: []Attempt{{Number: 1, PRNumber: 7}}}
	if err := state.Save(dir); err != nil {
		t.Fatal(err)
	}
	loaded := Load(dir)
	if got := loaded.Remediations["x"]; got == nil || got.Attempts[0].PRNumber != 7 {
		t.Fatalf("loaded = %+v", loaded)
	}
	if filepath.Base(filepath.Join(dir, FileName)) != FileName {
		t.Fatal("bad state filename")
	}
}

func TestEvidenceForPattern(t *testing.T) {
	pattern := models.PatternAnalysis{ID: "pattern", JobID: "job", Subject: "job", SharedRootCause: " timeout ", SharedBuilds: []string{"2"}}
	details := []models.JobDetail{{JobID: "job", Runs: []models.BuildResult{
		{BuildInfo: models.BuildInfo{BuildID: "1"}, TestCases: []models.TestCase{{Name: "old", Status: "failed"}}},
		{BuildInfo: models.BuildInfo{BuildID: "2"}, TestCases: []models.TestCase{{Name: "test", SuiteName: "suite", ClassName: "class", Status: "failed", FailureMessage: "timed out after 42 seconds", JUnitFile: "junit.xml"}}},
	}}}
	got := EvidenceForPattern(pattern, details)
	if got.PatternID != "pattern" || got.BuildWatermark != "2" || len(got.Tests) != 1 {
		t.Fatalf("evidence = %+v", got)
	}
	if got.Tests[0].Identity != "suite\x00class\x00test" || got.Tests[0].ErrorHash == "" {
		t.Fatalf("test evidence = %+v", got.Tests[0])
	}
}

func TestStateSaveWritesRedactedPublicProjection(t *testing.T) {
	dir := t.TempDir()
	state := NewState()
	state.Remediations["x"] = &Remediation{ID: "x", JobID: "job", Attempts: []Attempt{{
		Number: 1, URL: "https://github.com/o/r/pull/7", Status: StatusOpen, PatchHash: "secret-hash",
	}}}
	if err := state.Save(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, PublicFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-hash") || !strings.Contains(string(data), "pull/7") {
		t.Fatalf("public state = %s", data)
	}
}

func TestPublicProjectionUsesCurrentFindingID(t *testing.T) {
	state := NewState()
	state.Remediations["old"] = &Remediation{ID: "old", FindingID: "current"}
	public := state.Public()
	if _, ok := public.Remediations["current"]; !ok {
		t.Fatalf("public = %+v", public)
	}
	if _, ok := public.Remediations["old"]; ok {
		t.Fatalf("stale finding key published: %+v", public)
	}
}

func TestUntrackedPatternsExcludesEvidenceMatchedAttempt(t *testing.T) {
	pattern := models.PatternAnalysis{ID: "new", JobID: "job", Subject: "job", SharedBuilds: []string{"2"}}
	details := []models.JobDetail{{JobID: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "2"},
		TestCases: []models.TestCase{{Name: "test", Status: "failed", FailureMessage: "same"}},
	}}}}
	evidence := EvidenceForPattern(pattern, details)
	state := NewState()
	state.Remediations["old"] = &Remediation{
		ID: "old", FindingID: "old", JobID: "job", Evidence: evidence,
		Attempts: []Attempt{{Number: 1, Status: StatusStillFailingSameCause}},
	}
	if got := UntrackedPatterns(state, []models.PatternAnalysis{pattern}, details); len(got) != 0 {
		t.Fatalf("untracked = %+v", got)
	}
}

func TestUntrackedPatternsKeepsDistinctCauseOnSameJob(t *testing.T) {
	oldPattern := models.PatternAnalysis{ID: "old", JobID: "job", Subject: "job", SharedBuilds: []string{"1"}}
	newPattern := models.PatternAnalysis{ID: "new", JobID: "job", Subject: "job", SharedBuilds: []string{"2"}}
	details := []models.JobDetail{{JobID: "job", Runs: []models.BuildResult{
		{BuildInfo: models.BuildInfo{BuildID: "1"}, TestCases: []models.TestCase{{Name: "old-test", Status: "failed", FailureMessage: "old"}}},
		{BuildInfo: models.BuildInfo{BuildID: "2"}, TestCases: []models.TestCase{{Name: "new-test", Status: "failed", FailureMessage: "new"}}},
	}}}
	state := NewState()
	state.Remediations["old"] = &Remediation{
		ID: "old", FindingID: "old", JobID: "job", Evidence: EvidenceForPattern(oldPattern, details),
		Attempts: []Attempt{{Number: 1}},
	}
	if got := UntrackedPatterns(state, []models.PatternAnalysis{newPattern}, details); len(got) != 1 {
		t.Fatalf("untracked = %+v", got)
	}
}
