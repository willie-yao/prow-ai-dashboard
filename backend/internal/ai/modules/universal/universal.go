// Package universal provides the project-agnostic AI module: the only Module
// implementation. It performs NO upfront evidence fetching; the agentic loop
// discovers everything via registered tools (filesystem + k8s by default). The
// prompt is intentionally minimal, just enough context to point the agent at
// the right build and failing test.
package universal

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Module implements ai.Module for the universal agentic flow.
type Module struct{}

// New constructs the universal module.
func New() *Module { return &Module{} }

// Name returns "universal".
func (m *Module) Name() string { return "universal" }

// IsKnownTransient returns "" — universal mode leaves transient
// classification to either the optional ai.transient_patterns regex
// pre-filter (applied by the Service before reaching the module) or the
// AI's is_transient response field.
func (m *Module) IsKnownTransient(_ string) string { return "" }

// AnalysisPrompt builds a minimal per-failure prompt. No build log tail,
// no error grep, no machine logs — the agent is expected to fetch what it
// needs using list_artifacts / read_artifact / tail_artifact / grep_artifact
// and (when k8s tools are enabled) discover_clusters / discover_controllers.
//
// Trailing failure-body content is truncated to the last 8KB to keep the
// seed prompt small; the agent can tail_artifact the junit file itself if
// it needs more.
func (m *Module) AnalysisPrompt(_ context.Context, _ *http.Client, run *models.BuildResult, tc *models.TestCase, consecutive int) string {
	const failureBodyTail = 8 * 1024

	var sb strings.Builder
	fmt.Fprintf(&sb, "Test failure to investigate.\n\n")
	fmt.Fprintf(&sb, "Test name: %s\n", tc.Name)
	if tc.JUnitFile != "" {
		fmt.Fprintf(&sb, "JUnit file: %s\n", tc.JUnitFile)
	}
	if run != nil {
		if run.BuildID != "" {
			fmt.Fprintf(&sb, "Build: %s\n", run.BuildID)
		}
		if run.WebURL != "" {
			fmt.Fprintf(&sb, "Build URL: %s\n", run.WebURL)
		}
	}
	if consecutive > 1 {
		fmt.Fprintf(&sb, "Consecutive failures on this test: %d (persistent, not flaky).\n", consecutive)
	}

	if msg := strings.TrimSpace(tc.FailureMessage); msg != "" {
		sb.WriteString("\nFailure message:\n")
		sb.WriteString(msg)
		sb.WriteString("\n")
	}
	if body := strings.TrimSpace(tc.FailureBody); body != "" {
		sb.WriteString("\nFailure body (truncated to last 8KB):\n")
		if len(body) > failureBodyTail {
			body = body[len(body)-failureBodyTail:]
		}
		sb.WriteString(body)
		sb.WriteString("\n")
	}

	sb.WriteString(`
Use the available tools to investigate. Suggested starting points:
- list_artifacts("") to see the build's top-level layout
- tail_artifact("build-log.txt", 200) for the failing run's stdout
- grep_artifact for specific error strings you find

Tier-2 tools (when available) speed up Kubernetes-shaped digs:
- discover_clusters / list_cluster_machines / get_machine_log
- discover_controllers / get_controller_log

Cite the actual file paths and log lines that reveal the root cause. Do not
speculate. If the evidence is incomplete after exhausting reasonable tool
calls, say what you determined and what remains unknown.
`)

	return sb.String()
}
