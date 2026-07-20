package remediation

import (
	"context"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aggregator"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/junit"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

type fakeCompare struct {
	contains bool
	status   string
	err      error
}

func (f fakeCompare) CompareCommits(context.Context, string, string, string, string) (bool, string, error) {
	return f.contains, f.status, f.err
}

func periodicEvidence(failure string) Evidence {
	test := models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class"}
	return Evidence{BuildWatermark: "10", Tests: []TestEvidence{{
		Identity: junit.Identity(test), ErrorHash: aggregator.HashError(aggregator.NormalizeErrorMessage(failure)),
	}}}
}

func TestObservePeriodic_VerifiesTwoCleanRuns(t *testing.T) {
	remediation := &Remediation{JobID: "job", JobName: "job", JobType: models.JobTypePeriodic, SourceRepo: "o/r", CommitRepo: "o/r", Evidence: periodicEvidence("boom")}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "o/r"}
	pass := models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class", Status: "passed"}
	details := []models.JobDetail{{JobID: "job", Name: "job", Runs: []models.BuildResult{
		{BuildInfo: models.BuildInfo{BuildID: "12", Commit: "c2", Result: "SUCCESS", Passed: true}, TestCases: []models.TestCase{pass}},
		{BuildInfo: models.BuildInfo{BuildID: "11", Commit: "c1", Result: "SUCCESS", Passed: true}, TestCases: []models.TestCase{pass}},
	}}}
	if err := ObservePeriodic(context.Background(), fakeCompare{contains: true, status: "ahead"}, remediation, attempt, details, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusVerifiedFixed || attempt.Outcome != OutcomePassed {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodic_DetectsSameCause(t *testing.T) {
	failure := "boom 42"
	remediation := &Remediation{JobID: "job", JobName: "job", JobType: models.JobTypePeriodic, SourceRepo: "o/r", CommitRepo: "o/r", Evidence: periodicEvidence(failure)}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "o/r"}
	failed := models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class", Status: "failed", FailureMessage: failure}
	details := []models.JobDetail{{JobID: "job", Name: "job", Runs: []models.BuildResult{
		{BuildInfo: models.BuildInfo{BuildID: "11", Commit: "c1", Result: "FAILURE"}, TestCases: []models.TestCase{failed}},
	}}}
	if err := ObservePeriodic(context.Background(), fakeCompare{contains: true, status: "ahead"}, remediation, attempt, details, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusStillFailingSameCause {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodic_RejectsTargetMismatch(t *testing.T) {
	remediation := &Remediation{JobType: models.JobTypePeriodic, SourceRepo: "upstream/repo"}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "fork/repo"}
	if err := ObservePeriodic(context.Background(), fakeCompare{}, remediation, attempt, nil, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Outcome != OutcomeTargetRepoNotTested {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodicPreservesVerifiedOutcomeWhenOldRunsAgeOut(t *testing.T) {
	remediation := &Remediation{JobID: "job", JobName: "job", JobType: models.JobTypePeriodic, SourceRepo: "o/r", CommitRepo: "o/r", Evidence: periodicEvidence("boom")}
	attempt := &Attempt{
		Status: StatusVerifiedFixed, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "o/r",
		Observations: []BuildObservation{
			{BuildID: "11", JobType: models.JobTypePeriodic, Outcome: OutcomePassed},
			{BuildID: "12", JobType: models.JobTypePeriodic, Outcome: OutcomePassed},
		},
	}
	details := []models.JobDetail{{JobID: "job", Name: "job"}}
	if err := ObservePeriodic(context.Background(), fakeCompare{contains: true}, remediation, attempt, details, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusVerifiedFixed {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodicMissingSourceCommitIsInconclusive(t *testing.T) {
	remediation := &Remediation{JobID: "job", JobName: "job", JobType: models.JobTypePeriodic, SourceRepo: "o/r", CommitRepo: "o/r", Evidence: periodicEvidence("boom")}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "o/r"}
	details := []models.JobDetail{{JobID: "job", Name: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "11", Result: "SUCCESS", Passed: true},
	}}}}
	if err := ObservePeriodic(context.Background(), fakeCompare{}, remediation, attempt, details, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusInconclusive || attempt.Outcome != OutcomeInconclusive {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodicInconclusiveObservationStaysInconclusive(t *testing.T) {
	remediation := &Remediation{JobID: "job", JobName: "job", JobType: models.JobTypePeriodic, SourceRepo: "o/r", CommitRepo: "o/r", Evidence: periodicEvidence("boom")}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "o/r"}
	details := []models.JobDetail{{JobID: "job", Name: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "11", Commit: "new", Result: "SUCCESS", Passed: true},
	}}}}
	if err := ObservePeriodic(context.Background(), fakeCompare{contains: true, status: "ahead"}, remediation, attempt, details, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusInconclusive {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodicMissingJobDetailIsInconclusive(t *testing.T) {
	remediation := &Remediation{JobID: "job", JobName: "job", JobType: models.JobTypePeriodic, SourceRepo: "o/r", CommitRepo: "o/r"}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "o/r"}
	if err := ObservePeriodic(context.Background(), fakeCompare{}, remediation, attempt, nil, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusInconclusive {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePeriodicRejectsNonPrimaryRepoCommit(t *testing.T) {
	remediation := &Remediation{
		JobID: "job", JobName: "job", JobType: models.JobTypePeriodic,
		SourceRepo: "secondary/repo", CommitRepo: "primary/repo",
	}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, MergeSHA: "merge", TargetRepo: "secondary/repo"}
	if err := ObservePeriodic(context.Background(), fakeCompare{}, remediation, attempt, nil, 2); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusInconclusive || !strings.Contains(attempt.OutcomeReason, "primary/repo") {
		t.Fatalf("attempt = %+v", attempt)
	}
}
