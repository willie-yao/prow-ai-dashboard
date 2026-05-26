package aggregator

import (
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// setupTeardownPrefixes lists test name substrings that indicate
// setup/teardown tests which should be excluded unless they failed.
var setupTeardownPrefixes = []string{
	"SynchronizedBeforeSuite",
	"SynchronizedAfterSuite",
	"BeforeSuite",
	"AfterSuite",
}

// isSetupTeardown returns true if the test name looks like a setup/teardown test.
func isSetupTeardown(name string) bool {
	for _, prefix := range setupTeardownPrefixes {
		if strings.Contains(name, prefix) {
			return true
		}
	}
	return false
}

// BuildSearchIndex creates a searchable index of all unique test cases across all jobs.
func BuildSearchIndex(jobResults map[string][]models.BuildResult, jobs []models.ProwJob, now time.Time) models.SearchIndex {
	// Build a lookup from job name to ProwJob metadata.
	jobMeta := make(map[string]models.ProwJob, len(jobs))
	for _, j := range jobs {
		jobMeta[j.Name] = j
	}

	type testKey struct {
		testName string
		jobName  string
	}
	type testInfo struct {
		latestStatus string
		failures     int
		appearances  int
	}

	seen := make(map[testKey]*testInfo)

	for jobName, runs := range jobResults {
		// Process runs newest-first: the first occurrence sets the latest status.
		for _, run := range runs {
			for _, tc := range run.TestCases {
				key := testKey{testName: tc.Name, jobName: jobName}
				info, ok := seen[key]
				if !ok {
					info = &testInfo{latestStatus: tc.Status}
					seen[key] = info
				}
				if tc.Status == "skipped" {
					continue
				}
				info.appearances++
				if tc.Status == "failed" {
					info.failures++
				}
				// The first non-skipped status we encounter is from the latest run.
				if info.latestStatus == "skipped" {
					info.latestStatus = tc.Status
				}
			}
		}
	}

	var entries []models.SearchEntry

	// Add job-level entries (searchable by job name and tab name).
	for jobName := range jobResults {
		meta := jobMeta[jobName]
		entries = append(entries, models.SearchEntry{
			Kind:     "job",
			JobName:  jobName,
			TabName:  meta.TabName,
			Branch:   meta.Branch,
			Category: meta.Category,
			Status:   meta.TabName, // will be replaced below if we compute overall status
		})
	}

	for key, info := range seen {
		// Skip tests that were only ever skipped.
		if info.appearances == 0 {
			continue
		}

		// Skip setup/teardown tests unless they failed.
		if isSetupTeardown(key.testName) && info.failures == 0 {
			continue
		}

		var failRate float64
		if info.appearances > 0 {
			failRate = float64(info.failures) / float64(info.appearances)
		}

		meta := jobMeta[key.jobName]
		entries = append(entries, models.SearchEntry{
			Kind:     "test",
			TestName: key.testName,
			JobName:  key.jobName,
			TabName:  meta.TabName,
			Branch:   meta.Branch,
			Category: meta.Category,
			Status:   info.latestStatus,
			FailRate: failRate,
		})
	}

	// Sort by job name then test name for deterministic output.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].JobName != entries[j].JobName {
			return entries[i].JobName < entries[j].JobName
		}
		return entries[i].TestName < entries[j].TestName
	})

	return models.SearchIndex{
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Entries:     entries,
	}
}
