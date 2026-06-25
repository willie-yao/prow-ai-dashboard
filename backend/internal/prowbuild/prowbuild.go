// Package prowbuild addresses and reads a single Prow build's artifacts over a
// storage.Backend. It encodes only Prow's storage-layout conventions (which are
// the same on GCS, S3, or any bucket): the periodic logs/<job>/<build>/ and
// presubmit pr-logs/pull/<org_repo>/<pr#>/<job>/<build>/ paths. It does not know
// or care which storage provider backs the bucket.
package prowbuild

import (
	"fmt"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// JobLocation identifies which Prow-convention path a job lives under. For
// periodics, only JobType matters. For presubmits, Repo ("org/repo") is
// required to construct the pr-logs prefix; the pull number is per-build and
// lives on BuildLocation.
type JobLocation struct {
	JobType string // models.JobTypePeriodic or models.JobTypePresubmit
	Repo    string // org/repo for presubmits; ignored for periodics
}

// BuildLocation extends JobLocation with the per-build identifiers needed to
// address a specific build's artifacts.
type BuildLocation struct {
	JobLocation
	JobName    string
	BuildID    string
	PullNumber string // required for presubmits; ignored for periodics
}

// Build is a single Prow build's identity. PullNumber is the pull request
// number for presubmits and empty for periodics.
type Build struct {
	ID         string
	PullNumber string
}

// jobTypePrefix returns the bucket-relative prefix above the job name, trailing
// slash included. Examples:
//
//	periodic:  "logs/"
//	presubmit: "pr-logs/pull/<org_repo>/<pull>/"  (requires pullNumber)
//
// Panics if JobType is presubmit and Repo or pullNumber is empty; these are
// programming errors at construction time, not runtime user input.
func (l JobLocation) jobTypePrefix(pullNumber string) string {
	switch l.JobType {
	case models.JobTypePeriodic:
		return "logs/"
	case models.JobTypePresubmit:
		if l.Repo == "" {
			panic("prowbuild: presubmit JobLocation requires Repo (org/repo)")
		}
		if pullNumber == "" {
			panic("prowbuild: presubmit JobLocation requires pull number")
		}
		repo := strings.ReplaceAll(l.Repo, "/", "_")
		return "pr-logs/pull/" + repo + "/" + pullNumber + "/"
	default:
		panic(fmt.Sprintf("prowbuild: unknown JobType %q", l.JobType))
	}
}

// jobPath returns the bucket-relative path down to a job name, trailing slash
// included.
func (l JobLocation) jobPath(jobName, pullNumber string) string {
	return l.jobTypePrefix(pullNumber) + jobName + "/"
}

// BuildPath returns the bucket-relative path down to a specific build, trailing
// slash included.
func (l BuildLocation) BuildPath() string {
	return l.JobLocation.jobPath(l.JobName, l.PullNumber) + l.BuildID + "/"
}

// JobIndexPrefix returns the bucket-relative prefix for listing builds of a job
// WITHOUT a known pull number. For periodics this is "logs/<job>/" (the
// canonical location for all build IDs of the job). For presubmits this is
// "pr-logs/directory/<job>/", a Prow-maintained index containing one
// "<buildID>.txt" entry per build of the job across every pull request. The
// .txt body holds the actual "pr-logs/pull/<org_repo>/<pr#>/<job>/<buildID>".
func (l JobLocation) JobIndexPrefix(jobName string) string {
	switch l.JobType {
	case models.JobTypePeriodic:
		return "logs/" + jobName + "/"
	case models.JobTypePresubmit:
		return "pr-logs/directory/" + jobName + "/"
	default:
		panic(fmt.Sprintf("prowbuild: unknown JobType %q", l.JobType))
	}
}
