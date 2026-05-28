package capi

import (
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// Factory wires this AI module for fetcher.AIModuleRegistry. The cluster
// prefix comes from cfg.CAPI.ClusterNamePrefix when set; without it the
// module still works but its prompt is slightly less targeted.
func Factory(cfg *project.Config) ai.Module {
	prefix := ""
	if cfg.CAPI != nil {
		prefix = cfg.CAPI.ClusterNamePrefix
	}
	return New(prefix)
}
