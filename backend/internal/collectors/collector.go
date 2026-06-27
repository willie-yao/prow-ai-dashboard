// Package collectors defines the project-specific extension point for
// discovering per-test debug artifacts associated with a failed prow build.
//
// Concrete collectors live in subpackages such as collectors/generic and are
// selected by the project.yaml field artifacts.collector. The parent package
// intentionally does not import implementations.
package collectors

import (
	"context"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
)

// Collector populates per-test debug artifacts on a failed build's test cases.
// Implementations are tied to a specific project layout.
type Collector interface {
	// Name returns the registered collector name.
	Name() string

	// CollectArtifacts attaches debug artifacts to failed test cases.
	// loc carries the job and build identity needed for periodic and
	// presubmit artifact paths. Callers log errors, so implementations should
	// not abort the run on transient I/O.
	CollectArtifacts(ctx context.Context, loc prowbuild.BuildLocation, result *models.BuildResult) error
}
