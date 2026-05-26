// Package collectors defines the project-specific extension point for
// discovering per-test debug artifacts associated with a failed prow build.
//
// Concrete collectors live in sub-packages (e.g. collectors/capi,
// collectors/generic) and are selected by the project.yaml field
// artifacts.collector. The parent package intentionally does not import
// any implementation to avoid import cycles; selection is wired in
// cmd/fetcher/main.go.
package collectors

import (
	"context"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Collector populates per-test debug artifacts on a failed build's test cases.
// Implementations are tied to a specific project layout (e.g. CAPI's
// artifacts/clusters/{name}/machines/{vm}/... convention).
type Collector interface {
	// Name returns the registered name of the collector ("capi", "generic", ...).
	Name() string

	// CollectArtifacts attaches debug artifacts (e.g. ClusterArtifacts) to
	// failed test cases in result. Called once per failed build. Errors are
	// logged by the caller; implementations should not abort the run on
	// transient I/O failures.
	CollectArtifacts(ctx context.Context, jobName, buildID string, result *models.BuildResult) error
}
