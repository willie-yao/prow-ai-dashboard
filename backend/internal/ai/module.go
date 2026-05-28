package ai

import (
	"context"
	"net/http"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Module captures the project-specific AI knowledge required to analyze a test
// failure: transient-detection rules, evidence collection, and per-failure
// prompt construction. The system prompt is owned by the consumer repo and
// composed by the engine via ComposeSystemPrompt at fetcher startup, so it is
// not the module's concern.
type Module interface {
	// Name uniquely identifies the module. Used for logging and cache keys.
	Name() string

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

// AgenticPreferrer is an optional capability a Module may implement to opt
// specific failures into agentic (tool-calling) mode. Modules that do not
// implement it are treated as never preferring agentic mode; whether agentic
// runs in that case depends on the project's Agentic.Always config.
//
// The returned reason is logged when the module returns true so operators can
// understand why a particular failure went through the more expensive
// pipeline. An empty reason is replaced with "module preference" in logs.
type AgenticPreferrer interface {
	PrefersAgentic(run *models.BuildResult, tc *models.TestCase) (bool, string)
}
