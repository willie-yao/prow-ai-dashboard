// Package generic provides a project-agnostic AI module. It produces a generic
// prompt that does not assume Cluster API / Azure / Kubernetes-specific
// terminology and collects only the minimum evidence available from any prow
// test run (failure body + build log tail). Used as the default when no
// project-specific module is configured.
package generic

import (
	"context"
	"net/http"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/evidence"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Module implements ai.Module without any project-specific knowledge.
type Module struct{}

// New constructs the no-op generic module.
func New() *Module { return &Module{} }

// Name returns "generic".
func (m *Module) Name() string { return "generic" }

// IsKnownTransient always returns "" — the generic module relies on the AI to
// flag transients via the is_transient response field.
func (m *Module) IsKnownTransient(_ string) string { return "" }

// AnalysisPrompt builds a generic combined-analysis prompt using only the
// test failure body and (best-effort) the build log tail. Delegates the
// universal prelude (header + failure body + build log tail) to
// ai/evidence so every module that uses the curator path renders the same
// shape for cache stability.
func (m *Module) AnalysisPrompt(ctx context.Context, client *http.Client, run *models.BuildResult, tc *models.TestCase, consecutive int) string {
	var sb strings.Builder
	sb.WriteString(evidence.BuildPrelude(ctx, client, run, tc, consecutive, evidence.Options{}))

	sb.WriteString("\nPerform a complete investigation:\n")
	sb.WriteString("1. ROOT CAUSE: Find the specific error in the data above. Quote the actual error message or log line that reveals the failure. Do NOT speculate.\n")
	sb.WriteString("2. SUGGESTED FIX: Based on the root cause, give the specific fix.\n")
	sb.WriteString("3. SUMMARY: After finishing the investigation, write a 1-2 sentence headline summary that reflects your findings.\n")
	sb.WriteString("4. If artifacts show the cause clearly, state it with confidence. If evidence is incomplete, say what you determined and what remains unknown.\n")

	return sb.String()
}
