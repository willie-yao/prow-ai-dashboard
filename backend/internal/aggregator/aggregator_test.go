package aggregator

import (
	"fmt"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// helper to build a BuildResult with sensible defaults.
func makeBuild(id string, started time.Time, passed bool, tests []models.TestCase) models.BuildResult {
	dur := 300.0
	total := len(tests)
	p, f, s := 0, 0, 0
	for _, tc := range tests {
		switch tc.Status {
		case "passed":
			p++
		case "failed":
			f++
		case "skipped":
			s++
		}
	}
	result := "SUCCESS"
	if !passed {
		result = "FAILURE"
	}
	return models.BuildResult{
		BuildInfo: models.BuildInfo{
			BuildID:         id,
			JobName:         "test-job",
			Started:         started,
			Finished:        started.Add(time.Duration(dur) * time.Second),
			Passed:          passed,
			Result:          result,
			DurationSeconds: dur,
			Commit:          "abc123",
			ProwURL:         "https://prow.example.com/" + id,
			BuildLogURL:     "https://logs.example.com/" + id,
		},
		TestCases:    tests,
		TestsTotal:   total,
		TestsPassed:  p,
		TestsFailed:  f,
		TestsSkipped: s,
	}
}

var baseTime = time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

func hoursAgo(h int) time.Time {
	return baseTime.Add(-time.Duration(h) * time.Hour)
}

// ---------- ComputeJobSummary tests ----------

func TestComputeJobSummary_AllPassing(t *testing.T) {
	job := models.ProwJob{Name: "job-pass", Category: "e2e"}
	runs := []models.BuildResult{
		makeBuild("5", hoursAgo(1), true, nil),
		makeBuild("4", hoursAgo(2), true, nil),
		makeBuild("3", hoursAgo(3), true, nil),
		makeBuild("2", hoursAgo(4), true, nil),
	}

	s := ComputeJobSummary(job, runs, baseTime)

	if s.OverallStatus != "PASSING" {
		t.Errorf("expected PASSING, got %s", s.OverallStatus)
	}
	if s.LastRun == nil || s.LastRun.BuildID != "5" {
		t.Errorf("expected LastRun.BuildID=5, got %v", s.LastRun)
	}
	if len(s.RecentRuns) != 4 {
		t.Errorf("expected 4 recent runs, got %d", len(s.RecentRuns))
	}
}

func TestComputeJobSummary_AllFailing(t *testing.T) {
	job := models.ProwJob{Name: "job-fail"}
	runs := []models.BuildResult{
		makeBuild("3", hoursAgo(1), false, nil),
		makeBuild("2", hoursAgo(2), false, nil),
		makeBuild("1", hoursAgo(3), false, nil),
	}

	s := ComputeJobSummary(job, runs, baseTime)

	if s.OverallStatus != "FAILING" {
		t.Errorf("expected FAILING, got %s", s.OverallStatus)
	}
}

func TestComputeJobSummary_Mixed(t *testing.T) {
	job := models.ProwJob{Name: "job-flaky"}
	runs := []models.BuildResult{
		makeBuild("3", hoursAgo(1), true, nil),
		makeBuild("2", hoursAgo(2), false, nil),
		makeBuild("1", hoursAgo(3), true, nil),
	}

	s := ComputeJobSummary(job, runs, baseTime)

	if s.OverallStatus != "FLAKY" {
		t.Errorf("expected FLAKY, got %s", s.OverallStatus)
	}
}

func TestComputeJobSummary_EmptyRuns(t *testing.T) {
	job := models.ProwJob{Name: "job-empty"}
	s := ComputeJobSummary(job, nil, baseTime)

	if s.LastRun != nil {
		t.Error("expected nil LastRun for empty runs")
	}
	if len(s.RecentRuns) != 0 {
		t.Errorf("expected 0 recent runs, got %d", len(s.RecentRuns))
	}
	if s.PassRate7d != 0 || s.PassRate30d != 0 {
		t.Errorf("expected 0 pass rates, got 7d=%.2f 30d=%.2f", s.PassRate7d, s.PassRate30d)
	}
}

func TestComputeJobSummary_FewerThan3Runs_AllPass(t *testing.T) {
	job := models.ProwJob{Name: "job-two"}
	runs := []models.BuildResult{
		makeBuild("2", hoursAgo(1), true, nil),
		makeBuild("1", hoursAgo(2), true, nil),
	}

	s := ComputeJobSummary(job, runs, baseTime)

	if s.OverallStatus != "PASSING" {
		t.Errorf("expected PASSING with 2 passing runs, got %s", s.OverallStatus)
	}
}

func TestComputeJobSummary_FewerThan3Runs_OneFail(t *testing.T) {
	job := models.ProwJob{Name: "job-one-fail"}
	runs := []models.BuildResult{
		makeBuild("1", hoursAgo(1), false, nil),
	}

	s := ComputeJobSummary(job, runs, baseTime)

	// With only 1 run that failed, all checked runs fail → FAILING.
	if s.OverallStatus != "FAILING" {
		t.Errorf("expected FAILING with single failed run, got %s", s.OverallStatus)
	}
}

func TestComputeJobSummary_PassRates(t *testing.T) {
	job := models.ProwJob{Name: "job-rates"}
	now := baseTime
	runs := []models.BuildResult{
		// Within 7 days: 3 runs, 2 pass
		makeBuild("6", now.Add(-1*24*time.Hour), true, nil),
		makeBuild("5", now.Add(-2*24*time.Hour), false, nil),
		makeBuild("4", now.Add(-5*24*time.Hour), true, nil),
		// Within 30 days but outside 7 days: 2 runs, 1 pass
		makeBuild("3", now.Add(-10*24*time.Hour), false, nil),
		makeBuild("2", now.Add(-20*24*time.Hour), true, nil),
		// Outside 30 days
		makeBuild("1", now.Add(-60*24*time.Hour), true, nil),
	}

	s := ComputeJobSummary(job, runs, now)

	// 7d: 2/3 ≈ 0.6667
	if s.PassRate7d < 0.66 || s.PassRate7d > 0.67 {
		t.Errorf("expected PassRate7d ~0.6667, got %.4f", s.PassRate7d)
	}
	// 30d: 3 pass / 5 total = 0.6
	if s.PassRate30d < 0.59 || s.PassRate30d > 0.61 {
		t.Errorf("expected PassRate30d ~0.6, got %.4f", s.PassRate30d)
	}
}

func TestComputeJobSummary_RecentRunsCapped(t *testing.T) {
	job := models.ProwJob{Name: "job-many"}
	runs := make([]models.BuildResult, 25)
	for i := range runs {
		runs[i] = makeBuild(
			fmt.Sprintf("%d", 25-i),
			hoursAgo(i),
			true,
			nil,
		)
	}

	s := ComputeJobSummary(job, runs, baseTime)

	if len(s.RecentRuns) != 20 {
		t.Errorf("expected 20 recent runs (capped), got %d", len(s.RecentRuns))
	}
}

// ---------- BuildRunSummary tests ----------

func TestBuildRunSummary(t *testing.T) {
	br := makeBuild("42", baseTime, true, []models.TestCase{
		{Name: "t1", Status: "passed", DurationSeconds: 1.0},
		{Name: "t2", Status: "failed", DurationSeconds: 2.0, FailureMessage: "boom"},
	})

	rs := BuildRunSummary(br)

	if rs.BuildID != "42" {
		t.Errorf("BuildID mismatch: %s", rs.BuildID)
	}
	if !rs.Passed {
		t.Error("expected Passed=true")
	}
	if rs.TestsTotal != 2 || rs.TestsPassed != 1 || rs.TestsFailed != 1 {
		t.Errorf("test counts wrong: total=%d passed=%d failed=%d", rs.TestsTotal, rs.TestsPassed, rs.TestsFailed)
	}
}

// ---------- ClassifyFailure tests ----------

func makeTestCase(name, status, failMsg string) models.TestCase {
	return models.TestCase{
		Name:           name,
		Status:         status,
		DurationSeconds: 1.0,
		FailureMessage: failMsg,
	}
}

func TestClassifyFailure_Persistent(t *testing.T) {
	runs := []models.BuildResult{
		makeBuild("5", hoursAgo(1), false, []models.TestCase{makeTestCase("SomeTest", "failed", "timeout")}),
		makeBuild("4", hoursAgo(2), false, []models.TestCase{makeTestCase("SomeTest", "failed", "timeout")}),
		makeBuild("3", hoursAgo(3), false, []models.TestCase{makeTestCase("SomeTest", "failed", "timeout")}),
		makeBuild("2", hoursAgo(4), false, []models.TestCase{makeTestCase("SomeTest", "failed", "timeout")}),
		makeBuild("1", hoursAgo(5), false, []models.TestCase{makeTestCase("SomeTest", "failed", "timeout")}),
	}

	info := ClassifyFailure("SomeTest", runs, 3)

	if info.Classification != models.ClassificationPersistent {
		t.Errorf("expected persistent, got %s", info.Classification)
	}
	if info.ConsecutiveFailures != 5 {
		t.Errorf("expected 5 consecutive failures, got %d", info.ConsecutiveFailures)
	}
}

func TestClassifyFailure_Flaky(t *testing.T) {
	runs := []models.BuildResult{
		makeBuild("4", hoursAgo(1), false, []models.TestCase{makeTestCase("FlakyTest", "failed", "err")}),
		makeBuild("3", hoursAgo(2), true, []models.TestCase{makeTestCase("FlakyTest", "passed", "")}),
		makeBuild("2", hoursAgo(3), false, []models.TestCase{makeTestCase("FlakyTest", "failed", "err")}),
		makeBuild("1", hoursAgo(4), true, []models.TestCase{makeTestCase("FlakyTest", "passed", "")}),
	}

	info := ClassifyFailure("FlakyTest", runs, 3)

	if info.Classification != models.ClassificationFlaky {
		t.Errorf("expected flaky, got %s", info.Classification)
	}
}

func TestClassifyFailure_OneOff(t *testing.T) {
	runs := []models.BuildResult{
		makeBuild("3", hoursAgo(1), false, []models.TestCase{makeTestCase("OneOffTest", "failed", "blip")}),
		makeBuild("2", hoursAgo(2), true, []models.TestCase{makeTestCase("OneOffTest", "passed", "")}),
		makeBuild("1", hoursAgo(3), true, []models.TestCase{makeTestCase("OneOffTest", "passed", "")}),
	}

	info := ClassifyFailure("OneOffTest", runs, 3)

	if info.Classification != models.ClassificationOneOff {
		t.Errorf("expected one-off, got %s", info.Classification)
	}
}

func TestClassifyFailure_LatestPasses(t *testing.T) {
	runs := []models.BuildResult{
		makeBuild("3", hoursAgo(1), true, []models.TestCase{makeTestCase("GoodTest", "passed", "")}),
		makeBuild("2", hoursAgo(2), false, []models.TestCase{makeTestCase("GoodTest", "failed", "old")}),
		makeBuild("1", hoursAgo(3), true, []models.TestCase{makeTestCase("GoodTest", "passed", "")}),
	}

	info := ClassifyFailure("GoodTest", runs, 3)

	// consecutiveFailures == 0, only 1 historical failure → one-off
	if info.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 consecutive failures, got %d", info.ConsecutiveFailures)
	}
	if info.Classification != models.ClassificationOneOff {
		t.Errorf("expected one-off (single historical failure), got %s", info.Classification)
	}
}

// ---------- NormalizeErrorMessage tests ----------

func TestNormalizeErrorMessage_Timestamps(t *testing.T) {
	msg := "failed at 2026-03-15T10:30:00Z with error"
	got := NormalizeErrorMessage(msg)
	expected := "failed at <timestamp> with error"
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestNormalizeErrorMessage_NumericIDs(t *testing.T) {
	msg := "Expected 42 pods but got 3 pods"
	got := NormalizeErrorMessage(msg)
	expected := "Expected <num> pods but got <num> pods"
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestNormalizeErrorMessage_WhitespaceCollapsed(t *testing.T) {
	msg := "  too   many    spaces   "
	got := NormalizeErrorMessage(msg)
	expected := "too many spaces"
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestNormalizeErrorMessage_Combined(t *testing.T) {
	msg1 := "Expected <int>: 2 to equal <int>: 3"
	msg2 := "Expected <int>: 1 to equal <int>: 3"
	n1 := NormalizeErrorMessage(msg1)
	n2 := NormalizeErrorMessage(msg2)
	if n1 != n2 {
		t.Errorf("expected same normalized form, got %q vs %q", n1, n2)
	}
}

// ---------- HashError tests ----------

func TestHashError_SameInput(t *testing.T) {
	h1 := HashError("some error message")
	h2 := HashError("some error message")
	if h1 != h2 {
		t.Errorf("same input should produce same hash: %s vs %s", h1, h2)
	}
	if len(h1) != 8 {
		t.Errorf("expected 8 char hash, got %d chars: %s", len(h1), h1)
	}
}

func TestHashError_DifferentInput(t *testing.T) {
	h1 := HashError("error A")
	h2 := HashError("error B")
	if h1 == h2 {
		t.Errorf("different inputs should (almost certainly) produce different hashes: %s", h1)
	}
}
