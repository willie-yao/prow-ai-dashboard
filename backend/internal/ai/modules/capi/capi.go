// Package capi provides the AI module for Cluster API Provider projects
// (CAPZ, CAPV, CAPO, CAPI core, etc.). It contains the CAPI-specific
// transient-failure patterns and artifact evidence collection used to build
// per-failure user prompts. The system prompt is owned by the consumer repo
// and composed at fetcher startup; the module no longer contributes to it.
package capi

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// Module implements ai.Module for CAPI projects.
type Module struct {
	clusterPrefix string         // e.g. "capz-e2e"
	flavorRe      *regexp.Regexp // matches cluster names to extract the flavor
	evidence      project.EffectiveEvidence
}

// New constructs a CAPI AI module. clusterPrefix is the project's cluster name
// prefix (CAPZ uses "capz-e2e"); it is used to extract a flavor string from
// cluster names like "capz-e2e-prow-azl3-12345" -> "prow-azl3". evidence
// carries the consumer-declared (or default-filled) per-failure artifact
// sources to fetch.
func New(clusterPrefix string, evidence project.EffectiveEvidence) *Module {
	m := &Module{
		clusterPrefix: clusterPrefix,
		evidence:      evidence,
	}
	if clusterPrefix != "" {
		m.flavorRe = regexp.MustCompile(`^` + regexp.QuoteMeta(clusterPrefix) + `-(.+?)-\d`)
	}
	return m
}

// Name returns "capi". This is also used as the cache-key namespace.
func (m *Module) Name() string { return "capi" }

// AnalysisPrompt collects all configured CAPI artifact evidence for the
// given test case and builds the user message for a combined summary + deep
// root-cause analysis. Errors fetching individual artifacts are logged but
// do not abort.
func (m *Module) AnalysisPrompt(ctx context.Context, client *http.Client, run *models.BuildResult, tc *models.TestCase, consecutive int) string {
	ev := m.collectEvidence(ctx, client, run, tc, consecutive)
	return buildAnalysisPrompt(ev)
}

// flavor extracts the cluster flavor from a cluster name. Returns "" if no
// cluster name is set or no flavor regex is configured.
func (m *Module) flavor(tc *models.TestCase) string {
	if m.flavorRe == nil || tc.ClusterArtifacts == nil || tc.ClusterArtifacts.ClusterName == "" {
		return ""
	}
	if match := m.flavorRe.FindStringSubmatch(tc.ClusterArtifacts.ClusterName); len(match) > 1 {
		return match[1]
	}
	return ""
}

// buildAnalysisPrompt assembles the combined summary + root-cause prompt from
// collected evidence. The deep-analysis structural body is preserved from the
// pre-unification ComprehensiveAnalysis so stale "comprehensive:<hash>" cache
// entries unmarshal cleanly. The response JSON schema lives in the engine's
// shared ResponseFormatFooter (appended to every system prompt), so it is
// intentionally not repeated here.
func buildAnalysisPrompt(ev evidence) string {
	var sb strings.Builder
	sb.WriteString("Investigate this CAPI E2E test failure using the artifact data below.\n\n")
	fmt.Fprintf(&sb, "Test: %s\n", ev.TestName)
	if ev.ClusterFlavor != "" {
		fmt.Fprintf(&sb, "Flavor: %s\n", ev.ClusterFlavor)
	}
	fmt.Fprintf(&sb, "Failed %d consecutive times\n\n", ev.ConsecutiveCount)
	fmt.Fprintf(&sb, "Error: %s\n", ev.FailureMessage)

	if ev.FailureBody != "" {
		fmt.Fprintf(&sb, "\nStack trace:\n%s\n", truncate(ev.FailureBody, 5000))
	}

	if ev.BuildLogErrors != "" {
		fmt.Fprintf(&sb, "\n=== Build Log Errors ===\n%s\n", ev.BuildLogErrors)
	}
	if ev.BuildLogTail != "" {
		fmt.Fprintf(&sb, "\n=== Build Log (last 200 lines) ===\n%s\n", ev.BuildLogTail)
	}
	for _, rt := range sortedKeys(ev.ResourceYAMLs) {
		fmt.Fprintf(&sb, "\n=== %s Status ===\n%s\n", rt, ev.ResourceYAMLs[rt])
	}
	for _, name := range sortedKeys(ev.MachineLogs) {
		fmt.Fprintf(&sb, "\n=== Machine log: %s ===\n%s\n", name, ev.MachineLogs[name])
	}
	for _, ns := range sortedKeys(ev.ControllerLogs) {
		fmt.Fprintf(&sb, "\n=== Controller log: %s ===\n%s\n", ns, ev.ControllerLogs[ns])
	}
	if ev.ProviderActivityLog != "" {
		fmt.Fprintf(&sb, "\n=== Provider Activity Log ===\n%s\n", ev.ProviderActivityLog)
	}

	if len(ev.RequestedButMissing) > 0 {
		sb.WriteString("\n=== Configured but missing ===\n")
		sb.WriteString("The project requested these evidence sources but they were unavailable for this build. Do not infer a root cause from their absence:\n")
		for _, m := range ev.RequestedButMissing {
			fmt.Fprintf(&sb, "- %s\n", m)
		}
	}

	sb.WriteString("\nYou have the configured evidence sources available for this failure. Perform a complete investigation:\n")
	sb.WriteString("1. ROOT CAUSE: Find the specific error in the artifacts above. Quote the actual error message, status condition, or log line that reveals the failure. Do NOT speculate — cite what you found.\n")
	sb.WriteString("2. TRACE THE FAILURE: Use the project-specific knowledge in the system prompt above to identify which component or step failed. Cite the artifact (log file, status condition, etc.) that confirms it.\n")
	sb.WriteString("3. SUGGESTED FIX: Based on the root cause you identified, give the specific fix. Say exactly what file/config/setting needs to change and how. Do NOT say 'check the logs' — you already have them.\n")
	sb.WriteString("4. SUMMARY: After completing the root-cause investigation, write a 1-2 sentence headline summary that reflects your findings.\n")
	sb.WriteString("5. If artifacts show the cause clearly, state it with confidence. If evidence is incomplete, say what you determined and what remains unknown.\n")

	return sb.String()
}

// truncate returns the first max chars of s, suffixed with "..." if truncated.
// Mirrors the helper from the old internal/ai package so prompt output matches.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
