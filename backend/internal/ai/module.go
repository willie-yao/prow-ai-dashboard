package ai

import (
	"context"
	"net/http"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Module supplies the per-failure prompt and a stable name for cache keys.
// The system prompt is owned by the consumer repo and composed by the engine
// via ComposeSystemPrompt at fetcher startup, so it is not the module's
// concern. The only implementation is the project-agnostic universal module:
// the agentic loop discovers evidence itself via the registered tools, so the
// module performs no upfront evidence fetching.
type Module interface {
	// Name uniquely identifies the module. Used for logging and cache keys.
	Name() string

	// AnalysisPrompt returns the user message seeding a single agentic
	// analysis: the failing test's context. The agentic loop is expected to
	// fetch any further evidence via tools.
	AnalysisPrompt(ctx context.Context, client *http.Client, run *models.BuildResult, tc *models.TestCase, consecutive int) string
}
