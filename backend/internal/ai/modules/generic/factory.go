package generic

import (
	"log"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// Factory wires this AI module for fetcher.AIModuleRegistry.
//
// The generic module ignores the project's ai.evidence block (those fields
// describe CAPI-specific artifact paths). Warn at construction time so a
// consumer who sets evidence without switching modules notices the mismatch
// instead of silently getting no extra context in their prompts.
func Factory(cfg *project.Config) ai.Module {
	if cfg != nil && cfg.AI != nil && !cfg.AI.Evidence.IsZero() {
		log.Printf("WARN: ai.evidence is set but ai.module=%q ignores it (only \"capi\" reads evidence). Remove ai.evidence or switch ai.module to \"capi\".", cfg.AI.Module)
	}
	return New()
}
