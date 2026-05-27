// Package capi provides the AI module for Cluster API Provider projects
// (CAPZ, CAPV, CAPO, CAPI core, etc.). It contains the CAPI-specific system
// prompt, transient-failure patterns, prompt builders, and artifact evidence
// collection used to produce root-cause analyses for CAPI E2E failures.
package capi

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Module implements ai.Module for CAPI projects.
type Module struct {
	clusterPrefix string         // e.g. "capz-e2e"
	flavorRe      *regexp.Regexp // matches cluster names to extract the flavor
}

// New constructs a CAPI AI module. clusterPrefix is the project's cluster name
// prefix (CAPZ uses "capz-e2e"); it is used to extract a flavor string from
// cluster names like "capz-e2e-prow-azl3-12345" -> "prow-azl3".
func New(clusterPrefix string) *Module {
	m := &Module{clusterPrefix: clusterPrefix}
	if clusterPrefix != "" {
		m.flavorRe = regexp.MustCompile(`^` + regexp.QuoteMeta(clusterPrefix) + `-(.+?)-\d`)
	}
	return m
}

// Name returns "capi". This is also used as the cache-key namespace.
func (m *Module) Name() string { return "capi" }

// SystemPrompt returns the CAPI/Azure-aware system prompt.
func (m *Module) SystemPrompt() string { return systemPrompt }

// QuickSummaryPrompt builds the user message for a brief 1-2 sentence summary.
func (m *Module) QuickSummaryPrompt(testName, failureMessage, failureLocation string) string {
	return fmt.Sprintf(
		"Give a brief 1-2 sentence summary of why this CAPZ E2E test failed.\n\n"+
			"Test: %s\nError: %s\nLocation: %s\n\n"+
			"Respond in JSON: {\"summary\": \"...\", \"is_transient\": true/false}",
		testName, failureMessage, failureLocation,
	)
}

// DeepAnalysisPrompt collects all available CAPI artifact evidence for the
// given test case and builds the user message for a comprehensive root-cause
// analysis. Errors fetching individual artifacts are logged but do not abort.
func (m *Module) DeepAnalysisPrompt(ctx context.Context, client *http.Client, run *models.BuildResult, tc *models.TestCase, consecutive int) string {
	ev := m.collectEvidence(ctx, client, run, tc, consecutive)
	return buildDeepPrompt(ev)
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

// buildDeepPrompt assembles the comprehensive analysis prompt from collected
// evidence. The format string is preserved byte-for-byte from the pre-refactor
// ComprehensiveAnalysis so cached CAPZ analyses stay valid.
func buildDeepPrompt(ev evidence) string {
	var sb strings.Builder
	sb.WriteString("Investigate this CAPZ E2E test failure using the artifact data below.\n\n")
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
	if len(ev.ResourceYAMLs) > 0 {
		var resourceTypes []string
		for k := range ev.ResourceYAMLs {
			resourceTypes = append(resourceTypes, k)
		}
		sort.Strings(resourceTypes)
		for _, rt := range resourceTypes {
			fmt.Fprintf(&sb, "\n=== %s Status ===\n%s\n", rt, ev.ResourceYAMLs[rt])
		}
	}
	if ev.CloudInitLog != "" {
		fmt.Fprintf(&sb, "\n=== Cloud-Init Log ===\n%s\n", ev.CloudInitLog)
	}
	if ev.BootLog != "" {
		fmt.Fprintf(&sb, "\n=== Boot Log ===\n%s\n", ev.BootLog)
	}
	if ev.KubeletLog != "" {
		fmt.Fprintf(&sb, "\n=== Kubelet Log ===\n%s\n", ev.KubeletLog)
	}
	if ev.ContainerdLog != "" {
		fmt.Fprintf(&sb, "\n=== Containerd Log ===\n%s\n", ev.ContainerdLog)
	}
	if ev.JournalLog != "" {
		fmt.Fprintf(&sb, "\n=== Journal Log ===\n%s\n", ev.JournalLog)
	}
	if ev.AzureActivityLog != "" {
		fmt.Fprintf(&sb, "\n=== Azure Activity Log ===\n%s\n", ev.AzureActivityLog)
	}

	sb.WriteString("\nYou have been given ALL available artifacts for this failure. Perform a complete investigation:\n")
	sb.WriteString("1. ROOT CAUSE: Find the specific error in the artifacts above. Quote the actual error message, status condition, or log line that reveals the failure. Do NOT speculate — cite what you found.\n")
	sb.WriteString("2. TRACE THE CHAIN: Follow the dependency chain (VM provisioning → cloud-init → kubeadm → kubelet → CNI → CCM → providerID). Identify which step failed and why.\n")
	sb.WriteString("3. SUGGESTED FIX: Based on the root cause you identified, give the specific fix. Say exactly what file/config/setting needs to change and how. Do NOT say 'check the logs' — you already have them.\n")
	sb.WriteString("4. If artifacts show the cause clearly, state it with confidence. If evidence is incomplete, say what you determined and what remains unknown.\n\n")
	sb.WriteString(`Respond in JSON: {"root_cause": "the specific error found in evidence with quoted log lines", "severity": "Critical/High/Medium/Low", "suggested_fix": "exact fix with file paths and changes needed", "relevant_files": ["file1.go", "file2.yaml"]}`)

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
