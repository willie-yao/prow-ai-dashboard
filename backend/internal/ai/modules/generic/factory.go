package generic

import (
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// Factory wires this AI module for fetcher.AIModuleRegistry.
func Factory(_ *project.Config) ai.Module {
	return New()
}
