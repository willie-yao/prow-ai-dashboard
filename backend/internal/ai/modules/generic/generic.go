// Package generic provides a project-agnostic AI module. It produces a generic
// prompt that does not assume Cluster API / Azure / Kubernetes-specific
// terminology and collects only the minimum evidence available from any prow
// test run (failure body + build log tail). Used as the default when no
// project-specific module is configured.
package generic

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Module implements ai.Module without any project-specific knowledge.
type Module struct{}

// New constructs the no-op generic module.
func New() *Module { return &Module{} }

// Name returns "generic".
func (m *Module) Name() string { return "generic" }

// SystemPrompt returns a project-agnostic system message.
func (m *Module) SystemPrompt() string {
	return `You are an expert software test failure analyst. You diagnose failures from prow E2E test runs by analyzing error messages, stack traces, and build logs.

When analyzing failures:
- Focus on the first fatal error, not cascading symptoms.
- Distinguish between transient infrastructure issues and real bugs.
- Cite specific log lines or error messages rather than speculating.
- If evidence is incomplete, state what remains unknown rather than guessing.

Respond in JSON when asked.`
}

// IsKnownTransient always returns "" — the generic module relies on the AI to
// flag transients via the is_transient response field.
func (m *Module) IsKnownTransient(_ string) string { return "" }

// QuickSummaryPrompt builds a generic user message for a brief summary.
func (m *Module) QuickSummaryPrompt(testName, failureMessage, failureLocation string) string {
	return fmt.Sprintf(
		"Give a brief 1-2 sentence summary of why this test failed.\n\n"+
			"Test: %s\nError: %s\nLocation: %s\n\n"+
			"Respond in JSON: {\"summary\": \"...\", \"is_transient\": true/false}",
		testName, failureMessage, failureLocation,
	)
}

// DeepAnalysisPrompt builds a generic root-cause prompt using only the test
// failure body and (best-effort) the build log tail.
func (m *Module) DeepAnalysisPrompt(ctx context.Context, client *http.Client, run *models.BuildResult, tc *models.TestCase, consecutive int) string {
	var sb strings.Builder
	sb.WriteString("Investigate this test failure using the data below.\n\n")
	fmt.Fprintf(&sb, "Test: %s\n", tc.Name)
	fmt.Fprintf(&sb, "Failed %d consecutive times\n\n", consecutive)
	fmt.Fprintf(&sb, "Error: %s\n", tc.FailureMessage)

	if tc.FailureBody != "" {
		fmt.Fprintf(&sb, "\nStack trace:\n%s\n", truncate(tc.FailureBody, 5000))
	}

	if run.BuildLogURL != "" {
		if tail := fetchBuildLogTail(ctx, client, run.BuildLogURL); tail != "" {
			fmt.Fprintf(&sb, "\n=== Build Log (last 200 lines) ===\n%s\n", tail)
		}
	}

	sb.WriteString("\nPerform a complete investigation:\n")
	sb.WriteString("1. ROOT CAUSE: Find the specific error in the data above. Quote the actual error message or log line that reveals the failure. Do NOT speculate.\n")
	sb.WriteString("2. SUGGESTED FIX: Based on the root cause, give the specific fix.\n")
	sb.WriteString("3. If artifacts show the cause clearly, state it with confidence. If evidence is incomplete, say what you determined and what remains unknown.\n\n")
	sb.WriteString(`Respond in JSON: {"root_cause": "...", "severity": "Critical/High/Medium/Low", "suggested_fix": "...", "relevant_files": ["file1", "file2"]}`)

	return sb.String()
}

func fetchBuildLogTail(ctx context.Context, client *http.Client, url string) string {
	data, err := gcs.FetchRaw(ctx, client, url)
	if err != nil {
		log.Printf("  ⚠ generic evidence: failed to fetch build log: %v", err)
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 200 {
		lines = lines[len(lines)-200:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > 10000 {
		out = out[:10000] + "..."
	}
	return out
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
