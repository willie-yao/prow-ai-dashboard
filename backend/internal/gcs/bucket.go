package gcs

import (
	"fmt"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Bucket centralizes URL construction for a GCS bucket that holds Prow
// build logs. Two Prow conventions live in the same bucket:
//
//   periodic   logs/<job>/<build>/...
//   presubmit  pr-logs/pull/<org>_<repo>/<pr#>/<job>/<build>/...
//
// The legacy single-string helpers (ObjectURL, ObjectBaseURL, WebURL,
// ProwURL) interpret their path argument under the periodic "logs/"
// prefix and remain in use by the existing fetcher/collector pipeline.
// New code that needs presubmit support builds a BuildLocation and uses
// BuildObjectURL / BuildBaseURL / BuildWebURL / BuildProwURL. See Phase
// E Stage 2/3 for the path that wires presubmit fetching end-to-end.
type Bucket struct {
	Name string
}

// NewBucket returns a Bucket helper for the given GCS bucket.
func NewBucket(name string) *Bucket { return &Bucket{Name: name} }

// JobLocation identifies which Prow-convention path a job lives under.
// For periodics, only JobType matters. For presubmits, Repo ("org/repo")
// is required to construct the pr-logs prefix; the pull number is
// per-build and lives on BuildLocation.
type JobLocation struct {
	JobType string // models.JobTypePeriodic or models.JobTypePresubmit
	Repo    string // org/repo for presubmits; ignored for periodics
}

// BuildLocation extends JobLocation with the per-build identifiers needed
// to address a specific build's artifacts.
type BuildLocation struct {
	JobLocation
	JobName    string
	BuildID    string
	PullNumber string // required for presubmits; ignored for periodics
}

// jobTypePrefix returns the bucket-relative prefix above the job name.
// Trailing slash included. Examples:
//
//	periodic:  "logs/"
//	presubmit: "pr-logs/pull/<org_repo>/<pull>/"  (requires pullNumber)
//
// Panics if JobType is presubmit and Repo or pullNumber is empty; these
// are programming errors at construction time, not runtime user input.
func (l JobLocation) jobTypePrefix(pullNumber string) string {
	switch l.JobType {
	case models.JobTypePeriodic:
		return "logs/"
	case models.JobTypePresubmit:
		if l.Repo == "" {
			panic("gcs: presubmit JobLocation requires Repo (org/repo)")
		}
		if pullNumber == "" {
			panic("gcs: presubmit JobLocation requires pull number")
		}
		repo := strings.ReplaceAll(l.Repo, "/", "_")
		return "pr-logs/pull/" + repo + "/" + pullNumber + "/"
	default:
		panic(fmt.Sprintf("gcs: unknown JobType %q", l.JobType))
	}
}

// jobPath returns the bucket-relative path down to a job name, with a
// trailing slash. Examples:
//
//	periodic:  "logs/<job>/"
//	presubmit: "pr-logs/pull/<org_repo>/<pull>/<job>/"
func (l JobLocation) jobPath(jobName, pullNumber string) string {
	return l.jobTypePrefix(pullNumber) + jobName + "/"
}

// buildPath returns the bucket-relative path down to a specific build,
// with a trailing slash. Built from BuildLocation.
func (l BuildLocation) buildPath() string {
	return l.JobLocation.jobPath(l.JobName, l.PullNumber) + l.BuildID + "/"
}

// BuildObjectURL returns the raw GCS object URL for the given suffix
// under the build's artifact directory. Routes between periodic and
// presubmit layouts based on loc.JobType.
func (b *Bucket) BuildObjectURL(loc BuildLocation, suffix string) string {
	return fmt.Sprintf("https://storage.googleapis.com/%s/%s%s", b.Name, loc.buildPath(), suffix)
}

// BuildBaseURL returns the raw GCS prefix for the build's artifact
// directory, always trailing-slashed.
func (b *Bucket) BuildBaseURL(loc BuildLocation) string {
	return fmt.Sprintf("https://storage.googleapis.com/%s/%s", b.Name, loc.buildPath())
}

// BuildWebURL returns the human-browsable GCSweb URL for the build.
func (b *Bucket) BuildWebURL(loc BuildLocation) string {
	return fmt.Sprintf("https://gcsweb.k8s.io/gcs/%s/%s", b.Name, loc.buildPath())
}

// BuildProwURL returns the Prow UI URL for the build.
func (b *Bucket) BuildProwURL(loc BuildLocation) string {
	return fmt.Sprintf("https://prow.k8s.io/view/gs/%s/%s", b.Name, loc.buildPath())
}

// JobListPrefix returns the bucket-relative prefix for listing all build
// IDs of a specific job. For periodics that's "logs/<job>/". For
// presubmits it's "pr-logs/pull/<org_repo>/<pull>/<job>/", which lists
// builds for one PR. Listing across all PRs is a Stage 2/3 concern and
// requires a different prefix.
func (b *Bucket) JobListPrefix(loc JobLocation, jobName, pullNumber string) string {
	return loc.jobPath(jobName, pullNumber)
}

// ObjectURL returns the raw GCS object URL for the given path under the
// periodic "logs/" prefix. Periodic-only; for presubmits use
// BuildObjectURL with a presubmit BuildLocation.
//
//	NewBucket("kubernetes-ci-logs").ObjectURL("foo/1/build-log.txt") ->
//	  https://storage.googleapis.com/kubernetes-ci-logs/logs/foo/1/build-log.txt
func (b *Bucket) ObjectURL(path string) string {
	return fmt.Sprintf("https://storage.googleapis.com/%s/logs/%s", b.Name, path)
}

// ObjectBaseURL returns the raw GCS prefix for the given path under the
// periodic "logs/" prefix, always trailing-slashed. Periodic-only.
func (b *Bucket) ObjectBaseURL(path string) string {
	return fmt.Sprintf("https://storage.googleapis.com/%s/logs/%s", b.Name, ensureSlash(path))
}

// WebURL returns the human-browsable GCSweb URL for the given path under
// the periodic "logs/" prefix. Periodic-only.
func (b *Bucket) WebURL(path string) string {
	return fmt.Sprintf("https://gcsweb.k8s.io/gcs/%s/logs/%s", b.Name, path)
}

// ProwURL returns the Prow UI URL for the given path under the periodic
// "logs/" prefix. Periodic-only.
func (b *Bucket) ProwURL(path string) string {
	return fmt.Sprintf("https://prow.k8s.io/view/gs/%s/logs/%s", b.Name, path)
}

// ListAPIURL returns the GCS JSON API endpoint for listing objects in this bucket.
func (b *Bucket) ListAPIURL() string {
	return fmt.Sprintf("https://storage.googleapis.com/storage/v1/b/%s/o", b.Name)
}

func ensureSlash(s string) string {
	if s == "" {
		return ""
	}
	if s[len(s)-1] == '/' {
		return s
	}
	return s + "/"
}
