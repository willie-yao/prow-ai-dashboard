package aggregator

import (
	"sort"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

const maxFlakyResults = 50

// ComputeTestFlakiness computes flakiness stats for a single test across all runs of a job.
// Runs are expected newest-first.
func ComputeTestFlakiness(testName, jobID, jobName string, runs []models.BuildResult) models.TestFlakiness {
	tf := models.TestFlakiness{
		TestName: testName,
		JobName:  jobName,
		JobID:    jobID,
	}

	type testOutcome struct {
		passed  bool
		message string
		buildID string
		started time.Time
		dur     float64
	}

	// Collect outcomes for this test from each run (newest first).
	var outcomes []testOutcome
	for _, r := range runs {
		for _, tc := range r.TestCases {
			if tc.Name == testName {
				// Skip tests that were skipped — they're not pass or fail.
				if tc.Status == "skipped" {
					break
				}
				outcomes = append(outcomes, testOutcome{
					passed:  tc.Status == "passed",
					message: tc.FailureMessage,
					buildID: r.BuildID,
					started: r.Started,
					dur:     tc.DurationSeconds,
				})
				break
			}
		}
	}

	tf.TotalRuns = len(outcomes)
	if tf.TotalRuns == 0 {
		return tf
	}

	// Count passes, failures.
	for _, o := range outcomes {
		if o.passed {
			tf.Passes++
		} else {
			tf.Failures++
		}
	}

	// Fail rate.
	tf.FailRate = float64(tf.Failures) / float64(tf.TotalRuns)

	// Flip rate: count transitions / (total - 1).
	if tf.TotalRuns >= 2 {
		flips := 0
		for i := 1; i < len(outcomes); i++ {
			if outcomes[i].passed != outcomes[i-1].passed {
				flips++
			}
		}
		tf.FlipRate = float64(flips) / float64(tf.TotalRuns-1)
	}

	// Consecutive failures from the most recent run.
	for _, o := range outcomes {
		if !o.passed {
			tf.ConsecutiveFailures++
		} else {
			break
		}
	}

	// Classification via existing helper.
	info := ClassifyFailure(testName, runs, 3)
	tf.Classification = info.Classification

	// FirstFailedAt: timestamp of the earliest run in the current consecutive failure streak.
	if tf.ConsecutiveFailures > 0 {
		// outcomes[ConsecutiveFailures-1] is the oldest in the streak (newest-first order).
		tf.FirstFailedAt = outcomes[tf.ConsecutiveFailures-1].started.UTC().Format(time.RFC3339)
	}

	// LastFailure: most recent failed run.
	for _, o := range outcomes {
		if !o.passed {
			normalized := NormalizeErrorMessage(o.message)
			tf.LastFailure = &models.TestFailureInfo{
				BuildID:        o.buildID,
				Timestamp:      o.started.UTC().Format(time.RFC3339),
				FailureMessage: o.message,
				ErrorHash:      HashError(normalized),
			}
			break
		}
	}

	// Error patterns: group failures by normalized message.
	patternMap := make(map[string]*models.ErrorPattern)
	for _, o := range outcomes {
		if o.passed {
			continue
		}
		normalized := NormalizeErrorMessage(o.message)
		hash := HashError(normalized)
		if ep, ok := patternMap[hash]; ok {
			ep.Count++
		} else {
			patternMap[hash] = &models.ErrorPattern{
				NormalizedMessage: normalized,
				ErrorHash:         hash,
				Count:             1,
				ExampleMessage:    o.message,
			}
		}
	}
	for _, ep := range patternMap {
		tf.ErrorPatterns = append(tf.ErrorPatterns, *ep)
	}
	// Sort patterns by count descending for deterministic output.
	sort.Slice(tf.ErrorPatterns, func(i, j int) bool {
		if tf.ErrorPatterns[i].Count != tf.ErrorPatterns[j].Count {
			return tf.ErrorPatterns[i].Count > tf.ErrorPatterns[j].Count
		}
		return tf.ErrorPatterns[i].ErrorHash < tf.ErrorPatterns[j].ErrorHash
	})

	// Duration history (newest first, matching run order).
	for _, o := range outcomes {
		tf.DurationHistory = append(tf.DurationHistory, models.DurationPoint{
			BuildID:   o.buildID,
			Timestamp: o.started.UTC().Format(time.RFC3339),
			Duration:  o.dur,
			Passed:    o.passed,
		})
	}

	return tf
}

// ComputeFlakinessReport builds the full flakiness report across all jobs.
// jobResults is keyed by JobID. The jobs slice supplies JobID→Name lookup
// so emitted TestFlakiness entries carry both fields, matching what the
// search index and notification dedupe key expect.
func ComputeFlakinessReport(jobResults map[string][]models.BuildResult, jobs []models.ProwJob, now time.Time) models.FlakinessReport {
	jobName := make(map[string]string, len(jobs))
	for _, j := range jobs {
		jobName[j.JobID] = j.Name
	}

	var allFlaky []models.TestFlakiness

	for jobID, runs := range jobResults {
		// Collect all unique test names across runs.
		testSet := make(map[string]struct{})
		for _, r := range runs {
			for _, tc := range r.TestCases {
				testSet[tc.Name] = struct{}{}
			}
		}

		for testName := range testSet {
			tf := ComputeTestFlakiness(testName, jobID, jobName[jobID], runs)
			if tf.Failures > 0 {
				allFlaky = append(allFlaky, tf)
			}
		}
	}

	report := models.FlakinessReport{
		GeneratedAt:        now.UTC().Format(time.RFC3339),
		MostFlaky:          []models.TestFlakiness{},
		PersistentFailures: []models.TestFlakiness{},
		RecentlyBroken:     []models.TestFlakiness{},
	}

	// MostFlaky: only tests classified as "flaky", sorted by flip_rate desc.
	var mostFlaky []models.TestFlakiness
	for _, tf := range allFlaky {
		if tf.Classification == models.ClassificationFlaky {
			mostFlaky = append(mostFlaky, tf)
		}
	}
	sort.Slice(mostFlaky, func(i, j int) bool {
		if mostFlaky[i].FlipRate != mostFlaky[j].FlipRate {
			return mostFlaky[i].FlipRate > mostFlaky[j].FlipRate
		}
		return mostFlaky[i].FailRate > mostFlaky[j].FailRate
	})
	if len(mostFlaky) > maxFlakyResults {
		mostFlaky = mostFlaky[:maxFlakyResults]
	}
	report.MostFlaky = mostFlaky

	// PersistentFailures: consecutive_failures >= 3, sort desc.
	var persistent []models.TestFlakiness
	for _, tf := range allFlaky {
		if tf.ConsecutiveFailures >= 3 {
			persistent = append(persistent, tf)
		}
	}
	sort.Slice(persistent, func(i, j int) bool {
		if persistent[i].ConsecutiveFailures != persistent[j].ConsecutiveFailures {
			return persistent[i].ConsecutiveFailures > persistent[j].ConsecutiveFailures
		}
		return persistent[i].TestName < persistent[j].TestName
	})
	report.PersistentFailures = persistent

	// RecentlyBroken: first_failed_at within 48h of now, sort desc.
	cutoff := now.Add(-48 * time.Hour)
	var recentlyBroken []models.TestFlakiness
	for _, tf := range allFlaky {
		if tf.FirstFailedAt == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, tf.FirstFailedAt)
		if err != nil {
			continue
		}
		if !t.Before(cutoff) {
			recentlyBroken = append(recentlyBroken, tf)
		}
	}
	sort.Slice(recentlyBroken, func(i, j int) bool {
		// Sort by first_failed_at descending (most recent first).
		return recentlyBroken[i].FirstFailedAt > recentlyBroken[j].FirstFailedAt
	})
	report.RecentlyBroken = recentlyBroken

	return report
}
