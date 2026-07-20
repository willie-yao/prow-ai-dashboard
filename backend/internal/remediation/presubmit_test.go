package remediation

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aggregator"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/junit"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestObservePresubmits_PassesExactTest(t *testing.T) {
	failure := "timed out after 42 seconds"
	evidenceCase := models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class", FailureMessage: failure}
	identity := junit.Identity(evidenceCase)
	b := memoryBackend{objects: map[string]string{
		"pr-logs/pull/example_project/42/pull-e2e/10/started.json":        `{"timestamp":1}`,
		"pr-logs/pull/example_project/42/pull-e2e/10/finished.json":       `{"timestamp":2,"passed":true,"result":"SUCCESS","revision":"head"}`,
		"pr-logs/pull/example_project/42/pull-e2e/10/prowjob.json":        `{"spec":{"type":"presubmit","job":"pull-e2e","refs":{"org":"example","repo":"project","pulls":[{"number":42,"sha":"head"}]}},"status":{"state":"success","build_id":"10"}}`,
		"pr-logs/pull/example_project/42/pull-e2e/10/artifacts/junit.xml": `<testsuite name="suite"><testcase name="test" classname="class"/></testsuite>`,
	}}
	remediation := &Remediation{JobType: models.JobTypePeriodic, Evidence: Evidence{Tests: []TestEvidence{{
		Identity: identity, Name: "test", ErrorHash: aggregator.HashError(aggregator.NormalizeErrorMessage(failure)),
	}}}}
	attempt := &Attempt{PRNumber: 42, HeadSHA: "head", TargetRepo: "example/project", Status: StatusOpen}
	coverage := &CoverageCatalog{Complete: true, Tests: map[string][]VerificationJob{identity: {{JobID: "example/project/pull-e2e", JobName: "pull-e2e", Repo: "example/project"}}}}
	if err := ObservePresubmits(context.Background(), b, remediation, attempt, coverage); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusPremergeVerified || attempt.Outcome != OutcomePassed {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestClassifyObservation_SameCause(t *testing.T) {
	failure := "timed out after 42 seconds"
	test := models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class", Status: "failed", FailureMessage: failure}
	evidence := Evidence{Tests: []TestEvidence{{
		Identity: junit.Identity(test), ErrorHash: aggregator.HashError(aggregator.NormalizeErrorMessage(failure)),
	}}}
	var observation BuildObservation
	classifyObservation(evidence, []models.TestCase{test}, false, &observation)
	if observation.Outcome != OutcomeSameCause {
		t.Fatalf("observation = %+v", observation)
	}
}

func TestObservePresubmits_RejectsTargetMismatch(t *testing.T) {
	identity := "suite\x00class\x00test"
	remediation := &Remediation{Evidence: Evidence{Tests: []TestEvidence{{Identity: identity}}}}
	attempt := &Attempt{PRNumber: 42, HeadSHA: "head", TargetRepo: "fork/project", Status: StatusOpen}
	coverage := &CoverageCatalog{Complete: true, Tests: map[string][]VerificationJob{identity: {{JobID: "upstream/project/pull-e2e", JobName: "pull-e2e", Repo: "upstream/project"}}}}
	if err := ObservePresubmits(context.Background(), memoryBackend{}, remediation, attempt, coverage); err != nil {
		t.Fatal(err)
	}
	if attempt.Outcome != OutcomeTargetRepoNotTested {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestClassifyObservation_SkippedIsInconclusive(t *testing.T) {
	test := models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class", Status: "skipped"}
	evidence := Evidence{Tests: []TestEvidence{{Identity: junit.Identity(test)}}}
	var observation BuildObservation
	classifyObservation(evidence, []models.TestCase{test}, true, &observation)
	if observation.Outcome != OutcomeInconclusive {
		t.Fatalf("observation = %+v", observation)
	}
}

func TestObservePresubmitsFallsBackToFinishedRevision(t *testing.T) {
	failure := "boom"
	evidenceCase := models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class"}
	identity := junit.Identity(evidenceCase)
	b := memoryBackend{objects: map[string]string{
		"pr-logs/pull/example_project/42/pull-e2e/10/started.json":        `{"timestamp":1}`,
		"pr-logs/pull/example_project/42/pull-e2e/10/finished.json":       `{"timestamp":2,"passed":true,"result":"SUCCESS","revision":"head"}`,
		"pr-logs/pull/example_project/42/pull-e2e/10/artifacts/junit.xml": `<testsuite name="suite"><testcase name="test" classname="class"/></testsuite>`,
	}}
	remediation := &Remediation{Evidence: Evidence{Tests: []TestEvidence{{Identity: identity, ErrorHash: aggregator.HashError(aggregator.NormalizeErrorMessage(failure))}}}}
	attempt := &Attempt{PRNumber: 42, HeadSHA: "head", TargetRepo: "example/project", Status: StatusOpen}
	coverage := &CoverageCatalog{Complete: true, Tests: map[string][]VerificationJob{identity: {{JobID: "example/project/pull-e2e", JobName: "pull-e2e", Repo: "example/project"}}}}
	if err := ObservePresubmits(context.Background(), b, remediation, attempt, coverage); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusPremergeVerified {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePresubmitsSplitsEvidenceAcrossJobs(t *testing.T) {
	first := models.TestCase{Name: "first", SuiteName: "suite", ClassName: "class"}
	second := models.TestCase{Name: "second", SuiteName: "suite", ClassName: "class"}
	firstID, secondID := junit.Identity(first), junit.Identity(second)
	b := memoryBackend{objects: map[string]string{
		"pr-logs/pull/example_project/42/pull-a/10/started.json":        `{"timestamp":1}`,
		"pr-logs/pull/example_project/42/pull-a/10/finished.json":       `{"timestamp":2,"passed":true,"result":"SUCCESS","revision":"head"}`,
		"pr-logs/pull/example_project/42/pull-a/10/prowjob.json":        `{"spec":{"type":"presubmit","job":"pull-a","refs":{"pulls":[{"number":42,"sha":"head"}]}},"status":{"state":"success"}}`,
		"pr-logs/pull/example_project/42/pull-a/10/artifacts/junit.xml": `<testsuite name="suite"><testcase name="first" classname="class"/></testsuite>`,
		"pr-logs/pull/example_project/42/pull-b/11/started.json":        `{"timestamp":1}`,
		"pr-logs/pull/example_project/42/pull-b/11/finished.json":       `{"timestamp":2,"passed":true,"result":"SUCCESS","revision":"head"}`,
		"pr-logs/pull/example_project/42/pull-b/11/prowjob.json":        `{"spec":{"type":"presubmit","job":"pull-b","refs":{"pulls":[{"number":42,"sha":"head"}]}},"status":{"state":"success"}}`,
		"pr-logs/pull/example_project/42/pull-b/11/artifacts/junit.xml": `<testsuite name="suite"><testcase name="second" classname="class"/></testsuite>`,
	}}
	remediation := &Remediation{Evidence: Evidence{Tests: []TestEvidence{{Identity: firstID}, {Identity: secondID}}}}
	attempt := &Attempt{PRNumber: 42, HeadSHA: "head", TargetRepo: "example/project", Status: StatusOpen}
	coverage := &CoverageCatalog{Complete: true, Tests: map[string][]VerificationJob{
		firstID:  {{JobID: "example/project/pull-a", JobName: "pull-a", Repo: "example/project"}},
		secondID: {{JobID: "example/project/pull-b", JobName: "pull-b", Repo: "example/project"}},
	}}
	if err := ObservePresubmits(context.Background(), b, remediation, attempt, coverage); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusPremergeVerified || len(attempt.Observations) != 2 {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestApplyPresubmitOutcomeAcceptsOneApplicablePassingJob(t *testing.T) {
	remediation := &Remediation{}
	attempt := &Attempt{Status: StatusOpen}
	jobs := []VerificationJob{{JobName: "pull-a"}, {JobName: "pull-b"}}
	observations := []BuildObservation{{JobName: "pull-a", Outcome: OutcomePassed}}
	applyPresubmitOutcome(remediation, attempt, jobs, observations, true)
	if attempt.Status != StatusPremergeVerified {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestCurrentPresubmitObservationsUsesStoredCurrentHead(t *testing.T) {
	attempt := &Attempt{HeadSHA: "head", Observations: []BuildObservation{
		{JobName: "pull-a", JobType: models.JobTypePresubmit, HeadSHA: "head", Outcome: OutcomePassed},
		{JobName: "pull-a", JobType: models.JobTypePresubmit, HeadSHA: "old", Outcome: OutcomeSameCause},
	}}
	got := currentPresubmitObservations(attempt, []VerificationJob{{JobName: "pull-a"}})
	if len(got) != 1 || got[0].Outcome != OutcomePassed {
		t.Fatalf("observations = %+v", got)
	}
}

func TestCurrentPresubmitObservationsUsesNewestRerun(t *testing.T) {
	attempt := &Attempt{HeadSHA: "head", Observations: []BuildObservation{
		{BuildID: "10", JobName: "pull-a", JobType: models.JobTypePresubmit, HeadSHA: "head", Outcome: OutcomeSameCause},
		{BuildID: "11", JobName: "pull-a", JobType: models.JobTypePresubmit, HeadSHA: "head", Outcome: OutcomePassed},
	}}
	got := currentPresubmitObservations(attempt, []VerificationJob{{JobName: "pull-a"}})
	if len(got) != 1 || got[0].BuildID != "11" || got[0].Outcome != OutcomePassed {
		t.Fatalf("observations = %+v", got)
	}
	remediation := &Remediation{}
	attempt.Status, attempt.Outcome = StatusPresubmitFailedSameCause, OutcomeSameCause
	applyPresubmitOutcome(remediation, attempt, []VerificationJob{{JobName: "pull-a"}}, got, true)
	if attempt.Status != StatusPremergeVerified || attempt.Outcome != OutcomePassed {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestObservePresubmitsWithoutJobsIsInconclusive(t *testing.T) {
	remediation := &Remediation{JobType: models.JobTypePeriodic}
	attempt := &Attempt{PRNumber: 42, HeadSHA: "head", TargetRepo: "example/project", Status: StatusOpen}
	if err := ObservePresubmits(context.Background(), memoryBackend{}, remediation, attempt, &CoverageCatalog{Complete: true, Tests: map[string][]VerificationJob{}}); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusInconclusive {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestApplyPresubmitOutcomePreservesCompletedInconclusive(t *testing.T) {
	remediation := &Remediation{}
	attempt := &Attempt{Status: StatusOpen}
	applyPresubmitOutcome(remediation, attempt, []VerificationJob{{JobName: "pull-a"}}, []BuildObservation{{
		JobName: "pull-a", Outcome: OutcomeInconclusive, Reason: "no JUnit tests found",
	}}, true)
	if attempt.Status != StatusInconclusive || attempt.OutcomeReason != "no JUnit tests found" {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestApplyPresubmitOutcomeRejectsPartialCoveragePass(t *testing.T) {
	remediation := &Remediation{}
	attempt := &Attempt{Status: StatusOpen}
	applyPresubmitOutcome(remediation, attempt, []VerificationJob{{JobName: "pull-a"}}, []BuildObservation{{
		JobName: "pull-a", Outcome: OutcomePassed,
	}}, false)
	if attempt.Status != StatusInconclusive {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestMergeObservationsCapsHistory(t *testing.T) {
	attempt := &Attempt{}
	for i := 1; i <= 40; i++ {
		mergeObservations(attempt, []BuildObservation{{BuildID: fmt.Sprint(i), JobName: "job", JobType: models.JobTypePeriodic}})
	}
	if len(attempt.Observations) != maxStoredObservations {
		t.Fatalf("observations = %d", len(attempt.Observations))
	}
	if attempt.Observations[0].BuildID != "11" || attempt.Observations[len(attempt.Observations)-1].BuildID != "40" {
		t.Fatalf("observations = %+v", attempt.Observations)
	}
}

func TestApplyPresubmitOutcomePendingPrecedesPass(t *testing.T) {
	remediation := &Remediation{}
	attempt := &Attempt{Status: StatusOpen}
	applyPresubmitOutcome(remediation, attempt, []VerificationJob{{JobName: "a"}, {JobName: "b"}}, []BuildObservation{
		{JobName: "a", Outcome: OutcomePassed},
		{JobName: "b", Outcome: OutcomePending},
	}, true)
	if attempt.Status != StatusPresubmitRunning {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestClassifyObservationJobFailureCannotPass(t *testing.T) {
	test := models.TestCase{Name: "test", SuiteName: "suite", ClassName: "class", Status: "passed"}
	evidence := Evidence{Tests: []TestEvidence{{Identity: junit.Identity(test)}}}
	var observation BuildObservation
	classifyObservation(evidence, []models.TestCase{test}, false, &observation)
	if observation.Outcome != OutcomeDifferentCause {
		t.Fatalf("observation = %+v", observation)
	}
}

func TestClassifyObservationEmptyEvidenceIsInconclusive(t *testing.T) {
	var observation BuildObservation
	classifyObservation(Evidence{}, nil, true, &observation)
	if observation.Outcome != OutcomeInconclusive {
		t.Fatalf("observation = %+v", observation)
	}
}

func TestObservePresubmitsDoesNotFallbackOnInvalidProwJob(t *testing.T) {
	identity := "name\x00test"
	b := memoryBackend{objects: map[string]string{
		"pr-logs/pull/example_project/42/pull-e2e/10/started.json":        `{"timestamp":1}`,
		"pr-logs/pull/example_project/42/pull-e2e/10/finished.json":       `{"timestamp":2,"passed":true,"result":"SUCCESS","revision":"head"}`,
		"pr-logs/pull/example_project/42/pull-e2e/10/prowjob.json":        `{"spec":{"type":"presubmit","job":"other","refs":{"pulls":[{"number":42,"sha":"head"}]}}}`,
		"pr-logs/pull/example_project/42/pull-e2e/10/artifacts/junit.xml": `<testsuite><testcase name="test"/></testsuite>`,
	}}
	remediation := &Remediation{Evidence: Evidence{Tests: []TestEvidence{{Identity: identity}}}}
	attempt := &Attempt{PRNumber: 42, HeadSHA: "head", TargetRepo: "example/project", Status: StatusOpen}
	coverage := &CoverageCatalog{Complete: true, Tests: map[string][]VerificationJob{identity: {{JobID: "example/project/pull-e2e", JobName: "pull-e2e", Repo: "example/project"}}}}
	if err := ObservePresubmits(context.Background(), b, remediation, attempt, coverage); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != StatusInconclusive || !strings.Contains(attempt.OutcomeReason, "job is") {
		t.Fatalf("attempt = %+v", attempt)
	}
}
