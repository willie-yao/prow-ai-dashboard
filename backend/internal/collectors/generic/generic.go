// Package generic provides a no-op artifact collector for projects that do
// not have a project-specific artifact layout. The dashboard still works:
// failed tests simply have no ClusterArtifacts attached and the UI renders
// only the build log + JUnit data.
package generic

import (
	"context"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
)

// Collector is a no-op artifact collector.
type Collector struct{}

// New returns a generic no-op collector.
func New() *Collector { return &Collector{} }

// Name implements collectors.Collector.
func (*Collector) Name() string { return "generic" }

// CollectArtifacts implements collectors.Collector. It does nothing.
func (*Collector) CollectArtifacts(_ context.Context, _ prowbuild.BuildLocation, _ *models.BuildResult) error {
	return nil
}
