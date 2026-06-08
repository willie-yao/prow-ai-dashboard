// Package aggregator computes per-job and per-test aggregate statistics
// from build results, including pass rates, overall status, and failure
// classification (persistent vs flaky vs one-off).
package aggregator

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

const maxRecentRuns = 20

// Pass rate and overall status are computed over the most recent runs, matching
// how Prow/TestGrid summarize job health (rather than a wall-clock window).
const (
	passRateRecentRuns = 10  // number of recent runs the pass rate covers
	passingThreshold   = 0.9 // recent pass rate at/above this is PASSING
	failingThreshold   = 0.3 // recent pass rate at/below this is FAILING
)

// FailureInfo holds the result of classifying a test failure.
type FailureInfo struct {
	Classification      models.FailureClassification
	ConsecutiveFailures int
	ErrorHash           string
}

// ComputeJobSummary computes an aggregated JobSummary for a ProwJob given its
// build results (sorted newest-first).
func ComputeJobSummary(job models.ProwJob, runs []models.BuildResult) models.JobSummary {
	summary := models.JobSummary{
		ProwJob:    job,
		RecentRuns: []models.RunSummary{},
	}

	if len(runs) == 0 {
		return summary
	}

	// LastRun
	last := BuildRunSummary(runs[0])
	summary.LastRun = &last

	// RecentRuns (up to maxRecentRuns)
	limit := len(runs)
	if limit > maxRecentRuns {
		limit = maxRecentRuns
	}
	summary.RecentRuns = make([]models.RunSummary, limit)
	for i := 0; i < limit; i++ {
		summary.RecentRuns[i] = BuildRunSummary(runs[i])
	}

	// OverallStatus and pass rate both reflect the most recent runs (Prow-style)
	// rather than a wall-clock window, so infrequent jobs are judged on the same
	// number of runs as frequent ones.
	summary.OverallStatus = computeOverallStatus(runs)
	summary.PassRate7d = recentPassRate(runs, passRateRecentRuns)

	return summary
}

// computeOverallStatus classifies a job from its most recent runs using the
// pass rate over the last passRateRecentRuns runs:
//   - PASSING when the recent pass rate is at least passingThreshold
//   - FAILING when it is at or below failingThreshold
//   - FLAKY otherwise (a mix of passes and failures)
func computeOverallStatus(runs []models.BuildResult) string {
	if len(runs) == 0 {
		return "FLAKY"
	}
	rate := recentPassRate(runs, passRateRecentRuns)
	switch {
	case rate >= passingThreshold:
		return "PASSING"
	case rate <= failingThreshold:
		return "FAILING"
	default:
		return "FLAKY"
	}
}

// recentPassRate calculates the fraction of passing runs among the most recent
// n runs. Runs are expected newest-first. Returns 0 when there are no runs.
func recentPassRate(runs []models.BuildResult, n int) float64 {
	if len(runs) == 0 {
		return 0
	}
	if n > len(runs) {
		n = len(runs)
	}
	passed := 0
	for i := 0; i < n; i++ {
		if runs[i].Passed {
			passed++
		}
	}
	return float64(passed) / float64(n)
}

// BuildRunSummary converts a BuildResult into a compact RunSummary.
func BuildRunSummary(result models.BuildResult) models.RunSummary {
	return models.RunSummary{
		BuildID:         result.BuildID,
		Passed:          result.Passed,
		Timestamp:       result.Started,
		DurationSeconds: result.DurationSeconds,
		TestsTotal:      result.TestsTotal,
		TestsPassed:     result.TestsPassed,
		TestsFailed:     result.TestsFailed,
		TestsSkipped:    result.TestsSkipped,
	}
}

// ClassifyFailure examines the most recent runs to determine whether a test's
// failure is persistent, flaky, or a one-off. threshold is the number of
// consecutive failures required to be classified as persistent.
func ClassifyFailure(testName string, runs []models.BuildResult, threshold int) FailureInfo {
	if threshold <= 0 {
		threshold = 3
	}

	// Gather per-run pass/fail status for this specific test.
	type testOutcome struct {
		failed  bool
		message string
	}
	outcomes := make([]testOutcome, 0, len(runs))
	for _, r := range runs {
		for _, tc := range r.TestCases {
			if tc.Name == testName {
				if tc.Status == "skipped" {
					break
				}
				outcomes = append(outcomes, testOutcome{
					failed:  tc.Status == "failed",
					message: tc.FailureMessage,
				})
				break
			}
		}
	}

	if len(outcomes) == 0 {
		return FailureInfo{Classification: models.ClassificationOneOff}
	}

	// Count consecutive failures from the most recent run backwards.
	consecutiveFailures := 0
	var firstFailMsg string
	for _, o := range outcomes {
		if !o.failed {
			break
		}
		consecutiveFailures++
		if firstFailMsg == "" {
			firstFailMsg = o.message
		}
	}

	errHash := HashError(NormalizeErrorMessage(firstFailMsg))

	if consecutiveFailures >= threshold {
		return FailureInfo{
			Classification:      models.ClassificationPersistent,
			ConsecutiveFailures: consecutiveFailures,
			ErrorHash:           errHash,
		}
	}

	// Check for flakiness: if there's a mix of pass and fail in the outcomes.
	failCount := 0
	passCount := 0
	for _, o := range outcomes {
		if o.failed {
			failCount++
		} else {
			passCount++
		}
	}

	// One-off: failed exactly once in recent history.
	if failCount == 1 {
		return FailureInfo{
			Classification:      models.ClassificationOneOff,
			ConsecutiveFailures: consecutiveFailures,
			ErrorHash:           errHash,
		}
	}

	// Flaky: mix of passes and failures.
	if failCount > 0 && passCount > 0 {
		return FailureInfo{
			Classification:      models.ClassificationFlaky,
			ConsecutiveFailures: consecutiveFailures,
			ErrorHash:           errHash,
		}
	}

	return FailureInfo{
		Classification:      models.ClassificationOneOff,
		ConsecutiveFailures: consecutiveFailures,
		ErrorHash:           errHash,
	}
}

// numericRegex matches integers and decimal numbers.
var numericRegex = regexp.MustCompile(`\b\d[\d.]*\b`)

// timestampRegex matches common timestamp patterns like 2026-03-15T10:30:00Z.
var timestampRegex = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}[^\s]*`)

// whitespaceRegex matches runs of whitespace.
var whitespaceRegex = regexp.MustCompile(`\s+`)

// NormalizeErrorMessage normalizes an error message for similarity comparison.
func NormalizeErrorMessage(msg string) string {
	s := strings.TrimSpace(msg)
	// Replace timestamps first (they contain numbers).
	s = timestampRegex.ReplaceAllString(s, "<timestamp>")
	// Replace remaining numeric values.
	s = numericRegex.ReplaceAllString(s, "<num>")
	// Collapse whitespace.
	s = whitespaceRegex.ReplaceAllString(s, " ")
	return s
}

// HashError returns the first 8 hex characters of the SHA-256 hash of
// the given normalised message, for use as a deduplication key.
func HashError(normalizedMsg string) string {
	h := sha256.Sum256([]byte(normalizedMsg))
	return fmt.Sprintf("%x", h[:4])
}
