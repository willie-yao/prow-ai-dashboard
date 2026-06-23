// Package models defines the shared data types for the CAPZ Prow Dashboard.
package models

import (
	"time"
)

// JobType identifies how a Prow job is triggered, which also determines
// where its build artifacts live in the GCS bucket.
const (
	JobTypePeriodic  = "periodic"
	JobTypePresubmit = "presubmit"
)

// ProwJob represents a prow job definition parsed from test-infra YAML configs.
type ProwJob struct {
	Name            string `json:"name" yaml:"name"`
	TabName         string `json:"tab_name"`
	Category        string `json:"category"`
	Branch          string `json:"branch"`
	Description     string `json:"description"`
	MinimumInterval string `json:"minimum_interval" yaml:"minimum_interval"`
	Timeout         string `json:"timeout"`
	ConfigFile      string `json:"config_file"`
	// JobType is "periodic" or "presubmit". Always stamped by the parser.
	JobType string `json:"job_type"`
	// Repo is the "org/repo" the job runs against. Populated for presubmits
	// from the YAML map key; empty for periodics.
	Repo string `json:"repo"`
	// JobID is a stable identifier that uniquely distinguishes this job from
	// a same-named job in a different repo or job type. Computed via
	// JobIDFor(JobType, Repo, Name) at parse time and propagated to every
	// downstream wire type. The frontend uses this for routing and file
	// fetches; cache loaders use it as a map key.
	JobID string `json:"job_id"`
}

// JobIDFor builds a stable per-job identifier. Periodics use the bare name
// (Prow guarantees uniqueness within the periodics: list); presubmits use
// "<repo>/<name>" so same-named jobs in different repos do not collide in
// caches, search, flakiness, notifications, or AI cache entries.
func JobIDFor(jobType, repo, name string) string {
	if jobType == JobTypePresubmit {
		return repo + "/" + name
	}
	return name
}

// BuildInfo represents metadata for a single prow build.
type BuildInfo struct {
	BuildID         string    `json:"build_id"`
	JobName         string    `json:"job_name"`
	Started         time.Time `json:"started"`
	Finished        time.Time `json:"finished"`
	Passed          bool      `json:"passed"`
	Result          string    `json:"result"`
	DurationSeconds float64   `json:"duration_seconds"`
	Commit          string    `json:"commit"`
	RepoVersion     string    `json:"repo_version,omitempty"`
	ProwURL         string    `json:"prow_url"`
	BuildLogURL     string    `json:"build_log_url"`
	// JUnitURLs lists every junit*.xml under the build's artifacts/ dir,
	// discovered at fetch time. Empty when discovery failed or the build
	// has no junit output. Order is stable (sorted by full URL) so cache
	// reuse stays deterministic.
	JUnitURLs []string `json:"junit_urls,omitempty"`
	// PullNumber is the PR number that triggered this build for presubmits.
	// Empty for periodics. Required for reconstructing presubmit GCS paths
	// from cached BuildResults without reparsing the job config.
	PullNumber string `json:"pull_number,omitempty"`
	// WebURL is the human-clickable gcsweb directory for this build (e.g.
	// https://gcsweb.k8s.io/gcs/<bucket>/logs/<job>/<build>/ for periodics,
	// or .../pr-logs/pull/<org_repo>/<pr>/<job>/<build>/ for presubmits).
	// Populated by gcs.FetchBuildInfo so the frontend can link to artifact
	// directories without recomposing GCS paths from job identity.
	WebURL string `json:"web_url,omitempty"`
}

// AISummary is a brief AI-generated explanation of a test failure.
type AISummary struct {
	GeneratedAt string `json:"generated_at"`
	Summary     string `json:"summary"`
	IsTransient bool   `json:"is_transient"`
}

// AIAnalysis is a deep AI-generated root cause analysis for persistent failures.
type AIAnalysis struct {
	GeneratedAt string `json:"generated_at"`
	// Model is the provider's model identifier used for the analysis. Kept
	// in-memory for cache validation and debug logging, but never serialized
	// to public JSON so internal-only model labels do not leak via the
	// deployed GitHub Pages data files.
	Model         string   `json:"-"`
	RootCause     string   `json:"root_cause"`
	Severity      string   `json:"severity"` // Critical, High, Medium, Low, Transient-Ignore
	SuggestedFix  string   `json:"suggested_fix"`
	RelevantFiles []string `json:"relevant_files,omitempty"`
	// Mode records the analysis pipeline that produced this result. There is
	// a single pipeline ("agentic"); the field is retained so entries from a
	// prior pipeline are detected as stale and re-analyzed.
	Mode string `json:"mode,omitempty"`

	// ToolCalls is the number of agent tool invocations made during this
	// analysis.
	ToolCalls int `json:"tool_calls,omitempty"`
	// ModelBytes is the cumulative bytes sent to / received from the chat
	// completion endpoint.
	ModelBytes int `json:"model_bytes,omitempty"`
	// GCSBytes is the cumulative bytes fetched from GCS via agent tool
	// calls.
	GCSBytes int `json:"gcs_bytes,omitempty"`
	// ElapsedMs is the wall-clock duration of the analysis in milliseconds.
	ElapsedMs int `json:"elapsed_ms,omitempty"`
	// CacheHit reports whether the analysis was served from the AI cache.
	CacheHit bool `json:"cache_hit,omitempty"`
	// BudgetExhausted reports whether the agentic loop hit one of its
	// budget caps and was forced to finalize on best-effort evidence.
	BudgetExhausted bool `json:"budget_exhausted,omitempty"`

	// CritiquePassed reports whether this analysis cleared the critique
	// gate. Only meaningful when the project has critique enabled.
	CritiquePassed bool `json:"critique_passed,omitempty"`

	// CritiqueVersion records the critique-contract version under which
	// this analysis was validated. The build-level shouldReanalyze check
	// requires version >= the engine's current version when critique is
	// enabled, so strengthening the gate invalidates older entries.
	CritiqueVersion int `json:"critique_version,omitempty"`

	// SkillSetHash is the fingerprint of the consumer's loaded recipe
	// set at the time this analysis was validated. Edits to triggers /
	// required-evidence / procedure flip the hash and force re-analysis.
	// Empty when no recipes are loaded.
	SkillSetHash string `json:"skill_set_hash,omitempty"`
}

// TestCase represents a single test case from JUnit XML.
type TestCase struct {
	Name            string  `json:"name"`
	Status          string  `json:"status"` // "passed", "failed", "skipped"
	DurationSeconds float64 `json:"duration_seconds"`
	FailureMessage  string  `json:"failure_message,omitempty"`
	FailureBody     string  `json:"failure_body,omitempty"`
	FailureLocation string  `json:"failure_location,omitempty"`
	FailureLocURL   string  `json:"failure_location_url,omitempty"`
	// JUnitFile is the basename (within artifacts/) of the file this case
	// came from, e.g. "junit.e2e_suite.1.xml" or "junit_runner.xml". Lets
	// the UI disambiguate same-named cases that show up in different
	// shards or suites within one build.
	JUnitFile  string      `json:"junit_file,omitempty"`
	AISummary  *AISummary  `json:"ai_summary,omitempty"`
	AIAnalysis *AIAnalysis `json:"ai_analysis,omitempty"`
}

// BuildResult is a complete result for a single build: metadata + test cases.
type BuildResult struct {
	BuildInfo
	TestCases    []TestCase `json:"test_cases"`
	TestsTotal   int        `json:"tests_total"`
	TestsPassed  int        `json:"tests_passed"`
	TestsFailed  int        `json:"tests_failed"`
	TestsSkipped int        `json:"tests_skipped"`
}

// JobSummary represents aggregated data for a job on the landing page.
type JobSummary struct {
	ProwJob
	OverallStatus string       `json:"overall_status"` // "PASSING", "FAILING", "FLAKY"
	LastRun       *RunSummary  `json:"last_run,omitempty"`
	RecentRuns    []RunSummary `json:"recent_runs"`
	// PassRateRecent is the fraction of passing runs over the most recent runs.
	PassRateRecent float64 `json:"pass_rate_recent"`
}

// RunSummary is a compact summary of a single build run.
type RunSummary struct {
	BuildID         string    `json:"build_id"`
	Passed          bool      `json:"passed"`
	Result          string    `json:"result,omitempty"`
	Timestamp       time.Time `json:"timestamp"`
	DurationSeconds float64   `json:"duration_seconds,omitempty"`
	TestsTotal      int       `json:"tests_total,omitempty"`
	TestsPassed     int       `json:"tests_passed,omitempty"`
	TestsFailed     int       `json:"tests_failed,omitempty"`
	TestsSkipped    int       `json:"tests_skipped,omitempty"`
}

// Dashboard is the top-level structure for dashboard.json.
type Dashboard struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Jobs        []JobSummary `json:"jobs"`
}

// JobDetail is the per-job detail structure for jobs/{job-id}.json.
type JobDetail struct {
	Name    string        `json:"name"`
	JobID   string        `json:"job_id"`
	JobType string        `json:"job_type"`
	Repo    string        `json:"repo"`
	Runs    []BuildResult `json:"runs"`
}

// FailureClassification indicates the type of failure.
type FailureClassification string

const (
	ClassificationPersistent FailureClassification = "persistent"
	ClassificationFlaky      FailureClassification = "flaky"
	ClassificationOneOff     FailureClassification = "one-off"
)

// TestFlakiness represents flakiness statistics for a single test across all runs of a job.
type TestFlakiness struct {
	TestName            string                `json:"test_name"`
	JobName             string                `json:"job_name"`
	JobID               string                `json:"job_id"`
	TotalRuns           int                   `json:"total_runs"`
	Failures            int                   `json:"failures"`
	Passes              int                   `json:"passes"`
	FlipRate            float64               `json:"flip_rate"`
	FailRate            float64               `json:"fail_rate"`
	ConsecutiveFailures int                   `json:"consecutive_failures"`
	Classification      FailureClassification `json:"classification"`
	LastFailure         *TestFailureInfo      `json:"last_failure,omitempty"`
	FirstFailedAt       string                `json:"first_failed_at,omitempty"`
	ErrorPatterns       []ErrorPattern        `json:"error_patterns,omitempty"`
	DurationHistory     []DurationPoint       `json:"duration_history,omitempty"`
}

// TestFailureInfo captures the most recent failure details.
type TestFailureInfo struct {
	BuildID        string `json:"build_id"`
	Timestamp      string `json:"timestamp"`
	FailureMessage string `json:"failure_message"`
	ErrorHash      string `json:"error_hash"`
}

// ErrorPattern groups similar failures.
type ErrorPattern struct {
	NormalizedMessage string `json:"normalized_message"`
	ErrorHash         string `json:"error_hash"`
	Count             int    `json:"count"`
	ExampleMessage    string `json:"example_message"`
}

// DurationPoint is a single data point for duration trend charts.
type DurationPoint struct {
	BuildID   string  `json:"build_id"`
	Timestamp string  `json:"timestamp"`
	Duration  float64 `json:"duration"`
	Passed    bool    `json:"passed"`
}

// SearchEntry represents a searchable item (either a job or a test case).
type SearchEntry struct {
	Kind     string  `json:"kind"`      // "job" or "test"
	TestName string  `json:"test_name"` // empty for job entries
	JobName  string  `json:"job_name"`
	JobID    string  `json:"job_id"`
	JobType  string  `json:"job_type"`
	Repo     string  `json:"repo"`
	TabName  string  `json:"tab_name"`
	Branch   string  `json:"branch"`
	Category string  `json:"category"`
	Status   string  `json:"status"`    // overall status for jobs, test status for tests
	FailRate float64 `json:"fail_rate"` // from flakiness data if available
}

// SearchIndex is the top-level structure for search-index.json.
type SearchIndex struct {
	GeneratedAt string        `json:"generated_at"`
	Entries     []SearchEntry `json:"entries"`
}

// FlakinessReport is the top-level structure for flakiness.json.
type FlakinessReport struct {
	GeneratedAt        string          `json:"generated_at"`
	MostFlaky          []TestFlakiness `json:"most_flaky"`
	PersistentFailures []TestFlakiness `json:"persistent_failures"`
	RecentlyBroken     []TestFlakiness `json:"recently_broken"`
}
