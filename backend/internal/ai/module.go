package ai

import (
	"context"
	"net/http"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Module captures the project-specific AI knowledge required to analyze a test
// failure: system prompt, transient-detection rules, prompt construction, and
// artifact evidence collection. Each project plugs in its own Module so prompts
// and evidence selection stay coherent.
type Module interface {
	// Name uniquely identifies the module. Used for logging and cache keys.
	Name() string

	// SystemPrompt returns the system message sent with every chat completion.
	SystemPrompt() string

	// IsKnownTransient returns a non-empty reason if the failure message matches
	// a pattern the module considers a known transient (e.g. quota exhaustion).
	// Returning "" means "run normal AI analysis."
	IsKnownTransient(failureMessage string) string

	// AnalysisPrompt collects whatever artifact evidence the module needs and
	// returns the user message for a single combined summary + root-cause pass.
	// The model is expected to return JSON containing the summary, is_transient
	// flag, and the full deep-analysis fields.
	// Errors fetching individual artifacts should be logged but not returned;
	// the prompt should be built from whatever was available.
	AnalysisPrompt(ctx context.Context, client *http.Client, run *models.BuildResult, tc *models.TestCase, consecutive int) string
}
