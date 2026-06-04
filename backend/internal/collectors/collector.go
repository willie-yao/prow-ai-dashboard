// Package collectors defines the project-specific extension point for
// discovering per-test debug artifacts associated with a failed prow build.
//
// Concrete collectors live in sub-packages (e.g. collectors/generic) and are
// selected by the project.yaml field artifacts.collector. The parent package
// intentionally does not import any implementation to avoid import cycles;
// selection is wired in cmd/fetcher/main.go.
package collectors

import (
	"context"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Collector populates per-test debug artifacts on a failed build's test cases.
// Implementations are tied to a specific project layout.
type Collector interface {
	// Name returns the registered name of the collector (e.g. "generic").
	Name() string

	// CollectArtifacts attaches debug artifacts to failed test cases in
	// result. Called once per failed build. loc carries the JobType + Repo
	// + PullNumber needed to address build artifacts via gcs.Bucket helpers
	// across both periodic and presubmit layouts. Errors are logged by the
	// caller; implementations should not abort the run on transient I/O.
	CollectArtifacts(ctx context.Context, loc gcs.BuildLocation, result *models.BuildResult) error
}
