// Package models defines the shared data types for the CAPZ Prow Dashboard.
package models

import "time"

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
}

// BuildInfo represents metadata for a single prow build.
type BuildInfo struct {
	BuildID          string    `json:"build_id"`
	JobName          string    `json:"job_name"`
	Started          time.Time `json:"started"`
	Finished         time.Time `json:"finished"`
	Passed           bool      `json:"passed"`
	Result           string    `json:"result"`
	DurationSeconds  float64   `json:"duration_seconds"`
	Commit           string    `json:"commit"`
	RepoVersion      string    `json:"repo_version,omitempty"`
	ProwURL          string    `json:"prow_url"`
	BuildLogURL      string    `json:"build_log_url"`
	JUnitURL         string    `json:"junit_url,omitempty"`
}

// AISummary is a brief AI-generated explanation of a test failure.
type AISummary struct {
	GeneratedAt string `json:"generated_at"`
	Summary     string `json:"summary"`
	IsTransient bool   `json:"is_transient"`
}

// AIAnalysis is a deep AI-generated root cause analysis for persistent failures.
type AIAnalysis struct {
	GeneratedAt   string   `json:"generated_at"`
	Model         string   `json:"model"`
	RootCause     string   `json:"root_cause"`
	Severity      string   `json:"severity"` // Critical, High, Medium, Low, Transient-Ignore
	SuggestedFix  string   `json:"suggested_fix"`
	RelevantFiles []string `json:"relevant_files,omitempty"`
}

// TestCase represents a single test case from JUnit XML.
type TestCase struct {
	Name             string            `json:"name"`
	Status           string            `json:"status"` // "passed", "failed", "skipped"
	DurationSeconds  float64           `json:"duration_seconds"`
	FailureMessage   string            `json:"failure_message,omitempty"`
	FailureBody      string            `json:"failure_body,omitempty"`
	FailureLocation  string            `json:"failure_location,omitempty"`
	FailureLocURL    string            `json:"failure_location_url,omitempty"`
	ClusterArtifacts *ClusterArtifacts `json:"cluster_artifacts,omitempty"`
	AISummary        *AISummary        `json:"ai_summary,omitempty"`
	AIAnalysis       *AIAnalysis       `json:"ai_analysis,omitempty"`
}

// ClusterArtifacts holds links to debug artifacts for a specific cluster.
type ClusterArtifacts struct {
	ClusterName           string             `json:"cluster_name"`
	AzureActivityLog      string             `json:"azure_activity_log,omitempty"`
	Machines              []MachineArtifacts `json:"machines,omitempty"`
	PodLogDirs            map[string]string  `json:"pod_log_dirs,omitempty"` // name → GCSweb URL
	BootstrapResourcesURL string             `json:"bootstrap_resources_url,omitempty"`
}

// MachineArtifacts holds links to per-machine debug logs.
type MachineArtifacts struct {
	Name string            `json:"name"`
	Logs map[string]string `json:"logs"`
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
	OverallStatus string      `json:"overall_status"` // "PASSING", "FAILING", "FLAKY"
	LastRun       *RunSummary `json:"last_run,omitempty"`
	RecentRuns    []RunSummary `json:"recent_runs"`
	PassRate7d    float64     `json:"pass_rate_7d"`
	PassRate30d   float64     `json:"pass_rate_30d"`
}

// RunSummary is a compact summary of a single build run.
type RunSummary struct {
	BuildID         string    `json:"build_id"`
	Passed          bool      `json:"passed"`
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

// JobDetail is the per-job detail structure for jobs/{job-name}.json.
type JobDetail struct {
	Name string        `json:"name"`
	Runs []BuildResult `json:"runs"`
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
