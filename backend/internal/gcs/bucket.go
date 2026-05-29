package gcs

import (
	"fmt"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Bucket centralizes URL construction for a GCS bucket that holds Prow
// build logs. Two Prow conventions live in the same bucket:
//
//	periodic   logs/<job>/<build>/...
//	presubmit  pr-logs/pull/<org>_<repo>/<pr#>/<job>/<build>/...
//
// Callers construct a BuildLocation and use BuildObjectURL / BuildBaseURL
// / BuildWebURL / BuildProwURL so the same code path serves both
// conventions. JobIndexPrefix returns the right listing prefix for
// "list builds of this job" (logs/<job>/ vs pr-logs/directory/<job>/).
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

// BuildPath returns the bucket-relative path down to a specific build,
// with a trailing slash. Built from BuildLocation.
func (l BuildLocation) BuildPath() string {
	return l.JobLocation.jobPath(l.JobName, l.PullNumber) + l.BuildID + "/"
}

// BuildObjectURL returns the raw GCS object URL for the given suffix
// under the build's artifact directory. Routes between periodic and
// presubmit layouts based on loc.JobType.
func (b *Bucket) BuildObjectURL(loc BuildLocation, suffix string) string {
	return fmt.Sprintf("https://storage.googleapis.com/%s/%s%s", b.Name, loc.BuildPath(), suffix)
}

// BuildBaseURL returns the raw GCS prefix for the build's artifact
// directory, always trailing-slashed.
func (b *Bucket) BuildBaseURL(loc BuildLocation) string {
	return fmt.Sprintf("https://storage.googleapis.com/%s/%s", b.Name, loc.BuildPath())
}

// BuildWebURL returns the human-browsable GCSweb URL for the build.
func (b *Bucket) BuildWebURL(loc BuildLocation) string {
	return fmt.Sprintf("https://gcsweb.k8s.io/gcs/%s/%s", b.Name, loc.BuildPath())
}

// BuildProwURL returns the Prow UI URL for the build.
func (b *Bucket) BuildProwURL(loc BuildLocation) string {
	return fmt.Sprintf("https://prow.k8s.io/view/gs/%s/%s", b.Name, loc.BuildPath())
}

// JobIndexPrefix returns the bucket-relative prefix for listing builds
// of a job WITHOUT a known pull number. For periodics this is
// "logs/<job>/" (the canonical location for all build IDs of the job).
// For presubmits this is "pr-logs/directory/<job>/", a Prow-maintained
// index containing one "<buildID>.txt" entry per build of the job
// across every pull request. The .txt body holds the actual
// "pr-logs/pull/<org_repo>/<pr#>/<job>/<buildID>" path.
func (b *Bucket) JobIndexPrefix(loc JobLocation, jobName string) string {
	switch loc.JobType {
	case models.JobTypePeriodic:
		return "logs/" + jobName + "/"
	case models.JobTypePresubmit:
		return "pr-logs/directory/" + jobName + "/"
	default:
		panic(fmt.Sprintf("gcs: unknown JobType %q", loc.JobType))
	}
}

// ProwURL returns the Prow UI URL for the given path under the periodic
// "logs/" prefix. Periodic-only; used by the notifier for deep links in
// failure alerts (presubmit notifications would require a refactor to
// pass full BuildLocation through the notification state — see Phase E
// Stage 3 in the plan).
//
//	NewBucket("kubernetes-ci-logs").ProwURL("") ->
//	  https://prow.k8s.io/view/gs/kubernetes-ci-logs/logs/
func (b *Bucket) ProwURL(path string) string {
	return fmt.Sprintf("https://prow.k8s.io/view/gs/%s/logs/%s", b.Name, path)
}

// ListAPIURL returns the GCS JSON API endpoint for listing objects in this bucket.
func (b *Bucket) ListAPIURL() string {
	return fmt.Sprintf("https://storage.googleapis.com/storage/v1/b/%s/o", b.Name)
}
