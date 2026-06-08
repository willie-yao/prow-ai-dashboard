package ai

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
)

// Build-log triage pre-pass. Before the main agentic loop, a single scoped
// LLM call reads the tail of build-log.txt and distills the top-level error
// plus a suggested direction. The result seeds the loop's user prompt so a
// small-context model starts with the error already identified, instead of
// spending iterations (and context budget) rediscovering it. Opt-in via
// AgenticOptions.BuildLogTriage; degrades to a no-op if build-log.txt is
// missing or the call fails.

const (
	// triageTailLines / triageTailBytes bound the build-log slice fed to the
	// triage call. The ginkgo failure summary sits near the end of the log,
	// so a tail captures it while keeping the triage context small.
	triageTailLines = 400
	triageTailBytes = 48 * 1024

	triageBuildLogPath = "build-log.txt"
)

const triageSystemPrompt = `You are triaging a failed CI test run. You are given the tail of its build-log.txt, which usually ends with the test framework's failure summary.

Identify the single top-level error that failed the run and name the most likely subsystem, component, or artifact area to investigate next (for example a specific controller manager log, a machine's cloud-init/kubelet log, or a webhook). Be concise: 2 to 4 sentences. Quote the key error line. Do not speculate beyond what the log shows; if the log does not contain a clear error, say so.`

// runBuildLogTriage fetches the build-log tail and asks the model for a short
// top-level-error summary. Returns "" (and never an error to the caller) when
// triage cannot run, so the main loop proceeds with its normal seed prompt.
func (c *Client) runBuildLogTriage(ctx context.Context, browser artifacts.Browser) string {
	if browser == nil {
		return ""
	}
	tail, err := browser.Tail(ctx, triageBuildLogPath, triageTailLines, triageTailBytes)
	if err != nil || tail == nil || len(tail.Content) == 0 {
		log.Printf("  ⓘ build-log triage skipped: %s unavailable", triageBuildLogPath)
		return ""
	}

	messages := []agChatMessage{
		{Role: "system", Content: strPtr(triageSystemPrompt)},
		{Role: "user", Content: strPtr(fmt.Sprintf("Tail of %s:\n\n%s", triageBuildLogPath, tail.Content))},
	}
	resp, err := c.callChatWithTools(ctx, messages, nil)
	if err != nil || len(resp.Choices) == 0 || resp.Choices[0].Message.Content == nil {
		log.Printf("  ⓘ build-log triage call failed: %v", err)
		return ""
	}
	summary := strings.TrimSpace(*resp.Choices[0].Message.Content)
	if summary == "" {
		return ""
	}
	log.Printf("  🩺 build-log triage: %s", truncate(summary, 160))
	return summary
}

// withTriageSeed prepends a triage summary to the user prompt under a clear
// header so the agent treats it as a starting lead, not as ground truth.
func withTriageSeed(userPrompt, triage string) string {
	if strings.TrimSpace(triage) == "" {
		return userPrompt
	}
	return fmt.Sprintf("Build-log triage (automated pre-pass on %s; a starting lead, verify with the tools):\n%s\n\n---\n\n%s",
		triageBuildLogPath, triage, userPrompt)
}
