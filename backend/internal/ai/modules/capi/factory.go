package capi

import (
	"log"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// Factory wires this AI module for fetcher.AIModuleRegistry. The cluster
// prefix comes from cfg.CAPI.ClusterNamePrefix when set; without it the
// module still works but its prompt is slightly less targeted. The evidence
// config comes from cfg.AI.Evidence with engine defaults applied.
func Factory(cfg *project.Config) ai.Module {
	prefix := ""
	if cfg.CAPI != nil {
		prefix = cfg.CAPI.ClusterNamePrefix
	}
	ev, err := cfg.EffectiveEvidence()
	if err != nil {
		// project.Config.Validate runs at load time and surfaces evidence
		// regex errors there. Reaching this branch implies a programmer
		// bug; fall back to defaults so the dashboard keeps deploying.
		log.Printf("⚠ ai.capi: unexpected evidence config error: %v; using defaults", err)
		fallback := &project.Config{}
		ev, _ = fallback.EffectiveEvidence()
	}
	return New(prefix, ev)
}
