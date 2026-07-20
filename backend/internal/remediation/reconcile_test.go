package remediation

import (
	"context"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

type fakePRClient struct {
	pull ghpr.PullRequest
	err  error
}

func (f fakePRClient) GetPullRequest(context.Context, string, string, int) (ghpr.PullRequest, error) {
	return f.pull, f.err
}

func TestReconcileTracksMergedPullRequest(t *testing.T) {
	dir := t.TempDir()
	mergedAt := time.Date(2026, 7, 20, 2, 0, 0, 0, time.UTC)
	client := fakePRClient{pull: ghpr.PullRequest{
		Number: 7, HTMLURL: "https://github.com/o/r/pull/7", State: "closed", Merged: true,
		MergeCommitSHA: "merge", MergedAt: mergedAt,
		Head: ghpr.PullRequestRef{Repo: "fork/r", Ref: "fix", SHA: "head"},
		Base: ghpr.PullRequestRef{Repo: "o/r", Ref: "main", SHA: "base"},
	}}
	pattern := models.PatternAnalysis{ID: "pattern", JobID: "job", Subject: "job", SharedRootCause: "timeout"}
	details := []models.JobDetail{{JobID: "job", Name: "job", JobType: models.JobTypePeriodic}}
	state, err := NewReconciler(client, dir).Reconcile(context.Background(), []models.PatternAnalysis{pattern}, details,
		map[string]FixReference{"key": {URL: "https://github.com/o/r/pull/7", OpenedAt: "2026-07-20T01:00:00Z"}},
		func(models.PatternAnalysis) string { return "key" })
	if err != nil {
		t.Fatal(err)
	}
	attempt := state.Remediations["pattern"].Attempts[0]
	if attempt.Status != StatusMerged || attempt.MergeSHA != "merge" || attempt.HeadSHA != "head" || attempt.TargetRepo != "o/r" {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestParsePullRequestURL(t *testing.T) {
	owner, repo, number, err := ParsePullRequestURL("https://github.com/o/r/pull/12")
	if err != nil || owner != "o" || repo != "r" || number != 12 {
		t.Fatalf("owner=%q repo=%q number=%d err=%v", owner, repo, number, err)
	}
}

func TestApplyPullRequestMovesPremergeVerificationToMerged(t *testing.T) {
	entry := &Remediation{}
	attempt := &Attempt{Status: StatusPremergeVerified, HeadSHA: "head"}
	applyPullRequest(entry, attempt, ghpr.PullRequest{
		Number: 7, HTMLURL: "https://github.com/o/r/pull/7", State: "closed", Merged: true,
		MergeCommitSHA: "merge", Head: ghpr.PullRequestRef{SHA: "head"}, Base: ghpr.PullRequestRef{Repo: "o/r"},
	})
	if attempt.Status != StatusMerged || attempt.PRState != StatusMerged {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestReconcileRefreshesFindingIDFromMatchingEvidence(t *testing.T) {
	dir := t.TempDir()
	existing := NewState()
	existing.Remediations["old"] = &Remediation{
		ID: "old", FindingID: "old", JobID: "job",
		Evidence: Evidence{Tests: []TestEvidence{{Identity: "suite\x00class\x00test", ErrorHash: "hash"}}},
	}
	if err := existing.Save(dir); err != nil {
		t.Fatal(err)
	}
	pattern := models.PatternAnalysis{ID: "new", JobID: "job", Subject: "job", SharedBuilds: []string{"1"}}
	details := []models.JobDetail{{JobID: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "1"},
		TestCases: []models.TestCase{{Name: "test", SuiteName: "suite", ClassName: "class", Status: "failed", FailureMessage: "same"}},
	}}}}
	current := EvidenceForPattern(pattern, details)
	existing = Load(dir)
	existing.Remediations["old"].Evidence.Tests[0].ErrorHash = current.Tests[0].ErrorHash
	if err := existing.Save(dir); err != nil {
		t.Fatal(err)
	}
	client := fakePRClient{pull: ghpr.PullRequest{
		Number: 7, HTMLURL: "https://github.com/o/r/pull/7", State: "closed", Merged: true,
		Head: ghpr.PullRequestRef{SHA: "head"}, Base: ghpr.PullRequestRef{Repo: "o/r"}, MergeCommitSHA: "merge",
	}}
	state, err := NewReconciler(client, dir).Reconcile(context.Background(), []models.PatternAnalysis{pattern}, details, nil, func(models.PatternAnalysis) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if state.Remediations["old"].FindingID != "new" {
		t.Fatalf("remediation = %+v", state.Remediations["old"])
	}
}

func TestSourceRepoFromBucketMetadata(t *testing.T) {
	detail := models.JobDetail{Runs: []models.BuildResult{{BuildInfo: models.BuildInfo{
		RepoRefs: map[string]string{"example/project": "main"},
	}}}}
	if got := sourceRepoFromDetail(detail, ""); got != "example/project" {
		t.Fatalf("source repo = %q", got)
	}
}

type recoveringPRClient struct {
	fakePRClient
	url string
}

func (c recoveringPRClient) SearchPR(context.Context, string, string, string, string) (int, string, bool, error) {
	return 7, c.url, true, nil
}

func TestReconcileRecoversPullRequestFromMarker(t *testing.T) {
	dir := t.TempDir()
	client := recoveringPRClient{
		fakePRClient: fakePRClient{pull: ghpr.PullRequest{
			Number: 7, HTMLURL: "https://github.com/o/r/pull/7", State: "open",
			Head: ghpr.PullRequestRef{SHA: "head"}, Base: ghpr.PullRequestRef{Repo: "o/r"},
		}},
		url: "https://github.com/o/r/pull/7",
	}
	reconciler := NewReconciler(client, dir)
	reconciler.SetRecovery("o/r", client)
	pattern := models.PatternAnalysis{ID: "pattern", JobID: "job", Subject: "job", SharedRootCause: "timeout"}
	state, err := reconciler.Reconcile(context.Background(), []models.PatternAnalysis{pattern}, nil, nil, func(models.PatternAnalysis) string { return "key" })
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Remediations["pattern"]; got == nil || len(got.Attempts) != 1 || got.Attempts[0].PRNumber != 7 {
		t.Fatalf("state = %+v", state)
	}
}

func TestReconcileAppendsRetryToEvidenceMatchedRemediation(t *testing.T) {
	dir := t.TempDir()
	pattern := models.PatternAnalysis{ID: "new", JobID: "job", Subject: "job", SharedRootCause: "same", SharedBuilds: []string{"2"}}
	details := []models.JobDetail{{JobID: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "2"},
		TestCases: []models.TestCase{{Name: "test", SuiteName: "suite", ClassName: "class", Status: "failed", FailureMessage: "same"}},
	}}}}
	evidence := EvidenceForPattern(pattern, details)
	state := NewState()
	state.Remediations["old"] = &Remediation{
		ID: "old", FindingID: "old", JobID: "job", JobName: "job", Evidence: evidence,
		Attempts: []Attempt{{Number: 1, URL: "https://github.com/o/r/pull/1", Status: StatusStillFailingSameCause}},
	}
	if err := state.Save(dir); err != nil {
		t.Fatal(err)
	}
	client := fakePRClient{pull: ghpr.PullRequest{
		Number: 2, HTMLURL: "https://github.com/o/r/pull/2", State: "open",
		Head: ghpr.PullRequestRef{SHA: "head"}, Base: ghpr.PullRequestRef{Repo: "o/r"},
	}}
	state, err := NewReconciler(client, dir).Reconcile(context.Background(), []models.PatternAnalysis{pattern}, details,
		map[string]FixReference{"key": {URL: "https://github.com/o/r/pull/2"}}, func(models.PatternAnalysis) string { return "key" })
	if err != nil {
		t.Fatal(err)
	}
	if state.Remediations["new"] != nil || len(state.Remediations["old"].Attempts) != 2 || state.Remediations["old"].FindingID != "new" {
		t.Fatalf("state = %+v", state)
	}
}

func TestFinalizeMergedPresubmit(t *testing.T) {
	entry := &Remediation{JobType: models.JobTypePresubmit}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged, Outcome: OutcomePassed}
	finalizeMergedPresubmit(entry, attempt)
	if attempt.Status != StatusVerifiedFixed {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestFinalizeMergedPresubmitWithoutPassIsInconclusive(t *testing.T) {
	entry := &Remediation{JobType: models.JobTypePresubmit}
	attempt := &Attempt{Status: StatusMerged, PRState: StatusMerged}
	finalizeMergedPresubmit(entry, attempt)
	if attempt.Status != StatusInconclusive {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestApplyPullRequestClearsStaleEvidenceWhenMergedHeadChanges(t *testing.T) {
	entry := &Remediation{JobType: models.JobTypePresubmit}
	attempt := &Attempt{
		Status: StatusPremergeVerified, HeadSHA: "old", Outcome: OutcomePassed,
		Observations: []BuildObservation{{BuildID: "1", HeadSHA: "old", Outcome: OutcomePassed}},
	}
	applyPullRequest(entry, attempt, ghpr.PullRequest{
		Number: 7, HTMLURL: "https://github.com/o/r/pull/7", State: "closed", Merged: true,
		Head: ghpr.PullRequestRef{SHA: "new"}, Base: ghpr.PullRequestRef{Repo: "o/r"}, MergeCommitSHA: "merge",
	})
	if len(attempt.Observations) != 0 || attempt.Outcome != "" || attempt.Status != StatusMerged {
		t.Fatalf("attempt = %+v", attempt)
	}
	finalizeMergedPresubmit(entry, attempt)
	if attempt.Status != StatusInconclusive {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func TestReconcileReopensVerifiedFindingOnNewerFailure(t *testing.T) {
	dir := t.TempDir()
	pattern := models.PatternAnalysis{ID: "pattern", JobID: "job", Subject: "job", SharedBuilds: []string{"20"}}
	details := []models.JobDetail{{JobID: "job", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "20"},
		TestCases: []models.TestCase{{Name: "test", Status: "failed", FailureMessage: "same"}},
	}}}}
	current := EvidenceForPattern(pattern, details)
	state := NewState()
	previous := current
	previous.BuildWatermark = "10"
	state.Remediations["pattern"] = &Remediation{
		ID: "pattern", FindingID: "pattern", JobID: "job", Evidence: previous,
		Attempts: []Attempt{{Status: StatusVerifiedFixed, PRState: StatusMerged, PatchHash: "hash", URL: "https://github.com/o/r/pull/7"}},
	}
	if err := state.Save(dir); err != nil {
		t.Fatal(err)
	}
	client := fakePRClient{pull: ghpr.PullRequest{
		Number: 7, HTMLURL: "https://github.com/o/r/pull/7", State: "closed", Merged: true,
		Head: ghpr.PullRequestRef{SHA: "head"}, Base: ghpr.PullRequestRef{Repo: "o/r"}, MergeCommitSHA: "merge",
	}}
	state, err := NewReconciler(client, dir).Reconcile(context.Background(), []models.PatternAnalysis{pattern}, details, nil, func(models.PatternAnalysis) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	attempt := state.Remediations["pattern"].Attempts[0]
	if attempt.Status != StatusStillFailingSameCause || state.Remediations["pattern"].Evidence.BuildWatermark != "20" {
		t.Fatalf("attempt=%+v evidence=%+v", attempt, state.Remediations["pattern"].Evidence)
	}
}

func TestReconcilePreservesLinkedIssueLifecycleFields(t *testing.T) {
	dir := t.TempDir()
	state := NewState()
	state.Remediations["pattern"] = &Remediation{
		ID: "pattern", FindingID: "pattern", JobID: "job",
		Issue: &IssueRef{Number: 9, URL: "old", Repo: "o/r", State: "closed", LastTransition: "observing->verified_fixed"},
	}
	if err := state.Save(dir); err != nil {
		t.Fatal(err)
	}
	reconciler := NewReconciler(fakePRClient{}, dir)
	reconciler.SetIssues("o/r", map[string]IssueRef{"job": {Number: 9, URL: "new", Repo: "o/r"}}, nil)
	pattern := models.PatternAnalysis{ID: "pattern", JobID: "job", Subject: "job"}
	state, err := reconciler.Reconcile(context.Background(), []models.PatternAnalysis{pattern}, nil, nil, func(models.PatternAnalysis) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	issue := state.Remediations["pattern"].Issue
	if issue.URL != "new" || issue.State != "closed" || issue.LastTransition != "observing->verified_fixed" {
		t.Fatalf("issue = %+v", issue)
	}
}

func TestPruneTerminalRemediations(t *testing.T) {
	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	old := now.Add(-terminalRetention - time.Hour).Format(time.RFC3339)
	state := NewState()
	state.Remediations["remove"] = &Remediation{
		ID: "remove", FindingID: "remove", UpdatedAt: old,
		Attempts: []Attempt{{Status: StatusVerifiedFixed}},
	}
	state.Remediations["active"] = &Remediation{
		ID: "active", FindingID: "active", UpdatedAt: old,
		Attempts: []Attempt{{Status: StatusVerifiedFixed}},
	}
	state.Remediations["pending-issue"] = &Remediation{
		ID: "pending-issue", FindingID: "pending-issue", UpdatedAt: old,
		Issue: &IssueRef{Number: 1, State: "open"}, Attempts: []Attempt{{Status: StatusVerifiedFixed}},
	}
	pruneTerminalRemediations(state, []models.PatternAnalysis{{ID: "active"}}, now)
	if state.Remediations["remove"] != nil {
		t.Fatal("old terminal remediation was not pruned")
	}
	if state.Remediations["active"] == nil || state.Remediations["pending-issue"] == nil {
		t.Fatalf("retained state = %+v", state.Remediations)
	}
}
