// Package ai provides AI-powered failure analysis for CAPZ E2E tests using
// the GitHub Models inference API.
package ai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

const (
	// ModelsAPIURL is the GitHub Copilot chat completions endpoint.
	ModelsAPIURL = "https://api.githubcopilot.com/chat/completions"
	// DefaultModel is fast, used for quick summaries.
	DefaultModel = "claude-sonnet-4.5"
	// DeepModel is more capable, used for deep root-cause analysis.
	DeepModel = "claude-opus-4.6"

	callDelay = 500 * time.Millisecond
)

// Client calls the GitHub Models API for AI analysis.
type Client struct {
	httpClient *http.Client
	apiURL     string
	token      string
	cache      *Cache
}

// NewClient creates a new AI client with the given token and cache directory.
func NewClient(token string, cacheDir string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 60 * time.Second},
		apiURL:     ModelsAPIURL,
		token:      token,
		cache:      NewCache(cacheDir),
	}
}

// Cache returns the underlying cache so callers can persist it.
func (c *Client) Cache() *Cache {
	return c.cache
}

// ---------- Known transient detection ----------

// transientPattern defines a local pattern for known transient failures.
type transientPattern struct {
	match  func(string) bool
	reason string
}

var knownTransientPatterns = []transientPattern{
	{
		match:  func(s string) bool { return strings.Contains(s, "429") || strings.Contains(s, "throttling") || strings.Contains(s, "too many requests") },
		reason: "Azure API throttling (HTTP 429)",
	},
	{
		match:  func(s string) bool { return strings.Contains(s, "quota") && (strings.Contains(s, "exceeded") || strings.Contains(s, "limit")) },
		reason: "Azure resource quota exceeded",
	},
	{
		match: func(s string) bool {
			return strings.Contains(s, "context deadline exceeded") && (strings.Contains(s, "cleanup") || strings.Contains(s, "delete"))
		},
		reason: "Context deadline during cleanup",
	},
	{
		match: func(s string) bool {
			return strings.Contains(s, "dns") && (strings.Contains(s, "resolution") || strings.Contains(s, "lookup")) && strings.Contains(s, "failed")
		},
		reason: "DNS resolution failure",
	},
	{
		match:  func(s string) bool { return strings.Contains(s, "imagepullbackoff") },
		reason: "Image pull backoff (transient)",
	},
	{
		match:  func(s string) bool { return strings.Contains(s, "no space left on device") },
		reason: "Disk space exhausted",
	},
}

// IsKnownTransient checks if a failure message matches a known transient pattern
// that doesn't need AI analysis. Returns the reason if transient, empty string otherwise.
func IsKnownTransient(failureMessage string) string {
	lower := strings.ToLower(failureMessage)
	for _, p := range knownTransientPatterns {
		if p.match(lower) {
			return p.reason
		}
	}
	return ""
}

// ---------- Quick summary ----------

// QuickSummary generates a brief AI summary of a test failure.
func (c *Client) QuickSummary(ctx context.Context, testName, failureMessage, failureLocation string) (*models.AISummary, error) {
	key := cacheKey("summary", testName, failureMessage)

	if raw, ok := c.cache.Get(key); ok {
		var s models.AISummary
		if json.Unmarshal(raw, &s) == nil {
			return &s, nil
		}
	}

	userMsg := fmt.Sprintf(
		"Give a brief 1-2 sentence summary of why this CAPZ E2E test failed.\n\n"+
			"Test: %s\nError: %s\nLocation: %s\n\n"+
			"Respond in JSON: {\"summary\": \"...\", \"is_transient\": true/false}",
		testName, failureMessage, failureLocation,
	)

	resp, err := c.callAPI(ctx, DefaultModel, systemPrompt, userMsg)
	if err != nil {
		return nil, err
	}

	// Try to parse structured response.
	type summaryResponse struct {
		Summary     string `json:"summary"`
		IsTransient bool   `json:"is_transient"`
	}
	var parsed summaryResponse
	cleaned := extractJSON(resp)
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		// Fallback: use raw text, assume not transient.
		parsed = summaryResponse{Summary: resp, IsTransient: false}
	}

	summary := &models.AISummary{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Summary:     parsed.Summary,
		IsTransient: parsed.IsTransient,
	}

	_ = c.cache.Set(key, summary)
	return summary, nil
}

// ---------- Deep analysis ----------

// deepAnalysisResponse is the expected JSON structure from the deep model.
type deepAnalysisResponse struct {
	RootCause     string   `json:"root_cause"`
	Severity      string   `json:"severity"`
	SuggestedFix  string   `json:"suggested_fix"`
	RelevantFiles []string `json:"relevant_files"`
}

// DeepAnalysis generates a detailed root-cause analysis for persistent failures.
func (c *Client) DeepAnalysis(ctx context.Context, testName string, consecutiveFailures int, failureMessage, failureBody, buildLogTail, activityLogExcerpt string) (*models.AIAnalysis, error) {
	key := cacheKey("deep", testName, failureMessage)

	if raw, ok := c.cache.Get(key); ok {
		var a models.AIAnalysis
		if json.Unmarshal(raw, &a) == nil {
			return &a, nil
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Analyze this persistent CAPZ E2E test failure (failed %d consecutive times).\n\n", consecutiveFailures)
	fmt.Fprintf(&sb, "Test: %s\nError: %s\n", testName, failureMessage)
	if failureBody != "" {
		fmt.Fprintf(&sb, "\nFailure details:\n%s\n", truncate(failureBody, 5000))
	}
	if buildLogTail != "" {
		fmt.Fprintf(&sb, "\nBuild log (last lines):\n%s\n", truncate(buildLogTail, 5000))
	}
	if activityLogExcerpt != "" {
		fmt.Fprintf(&sb, "\nAzure activity log excerpt:\n%s\n", truncate(activityLogExcerpt, 3000))
	}
	sb.WriteString("\nRespond in JSON with fields: root_cause, severity, suggested_fix, relevant_files")

	resp, err := c.callAPI(ctx, DeepModel, systemPrompt, sb.String())
	if err != nil {
		return nil, err
	}

	var parsed deepAnalysisResponse
	cleaned := extractJSON(resp)
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		// If JSON parse fails, use the raw text as root cause.
		parsed = deepAnalysisResponse{
			RootCause:    resp,
			Severity:     "Medium",
			SuggestedFix: "Unable to parse structured response",
		}
	}

	analysis := &models.AIAnalysis{
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Model:         DeepModel,
		RootCause:     parsed.RootCause,
		Severity:      parsed.Severity,
		SuggestedFix:  parsed.SuggestedFix,
		RelevantFiles: parsed.RelevantFiles,
	}

	_ = c.cache.Set(key, analysis)
	return analysis, nil
}

// ---------- Comprehensive analysis ----------

// ComprehensiveAnalysis generates a thorough debugging analysis using all
// available artifact evidence. This replaces the old AnalysisParams-based version.
func (c *Client) ComprehensiveAnalysis(ctx context.Context, evidence Evidence) (*models.AIAnalysis, error) {
	key := comprehensiveCacheKey(evidence)

	if raw, ok := c.cache.Get(key); ok {
		var a models.AIAnalysis
		if json.Unmarshal(raw, &a) == nil {
			return &a, nil
		}
	}

	var sb strings.Builder
	sb.WriteString("Investigate this CAPZ E2E test failure using the artifact data below.\n\n")
	fmt.Fprintf(&sb, "Test: %s\n", evidence.TestName)
	if evidence.ClusterFlavor != "" {
		fmt.Fprintf(&sb, "Flavor: %s\n", evidence.ClusterFlavor)
	}
	fmt.Fprintf(&sb, "Failed %d consecutive times\n\n", evidence.ConsecutiveCount)
	fmt.Fprintf(&sb, "Error: %s\n", evidence.FailureMessage)

	if evidence.FailureBody != "" {
		fmt.Fprintf(&sb, "\nStack trace:\n%s\n", truncate(evidence.FailureBody, 5000))
	}

	if evidence.BuildLogErrors != "" {
		fmt.Fprintf(&sb, "\n=== Build Log Errors ===\n%s\n", evidence.BuildLogErrors)
	}
	if evidence.BuildLogTail != "" {
		fmt.Fprintf(&sb, "\n=== Build Log (last 200 lines) ===\n%s\n", evidence.BuildLogTail)
	}
	// Add all resource YAMLs dynamically
	if len(evidence.ResourceYAMLs) > 0 {
		// Sort keys for deterministic output
		var resourceTypes []string
		for k := range evidence.ResourceYAMLs {
			resourceTypes = append(resourceTypes, k)
		}
		sort.Strings(resourceTypes)
		for _, rt := range resourceTypes {
			fmt.Fprintf(&sb, "\n=== %s Status ===\n%s\n", rt, evidence.ResourceYAMLs[rt])
		}
	}
	if evidence.CloudInitLog != "" {
		fmt.Fprintf(&sb, "\n=== Cloud-Init Log ===\n%s\n", evidence.CloudInitLog)
	}
	if evidence.BootLog != "" {
		fmt.Fprintf(&sb, "\n=== Boot Log ===\n%s\n", evidence.BootLog)
	}
	if evidence.KubeletLog != "" {
		fmt.Fprintf(&sb, "\n=== Kubelet Log ===\n%s\n", evidence.KubeletLog)
	}
	if evidence.ContainerdLog != "" {
		fmt.Fprintf(&sb, "\n=== Containerd Log ===\n%s\n", evidence.ContainerdLog)
	}
	if evidence.JournalLog != "" {
		fmt.Fprintf(&sb, "\n=== Journal Log ===\n%s\n", evidence.JournalLog)
	}
	if evidence.AzureActivityLog != "" {
		fmt.Fprintf(&sb, "\n=== Azure Activity Log ===\n%s\n", evidence.AzureActivityLog)
	}

	sb.WriteString("\nYou have been given ALL available artifacts for this failure. Perform a complete investigation:\n")
	sb.WriteString("1. ROOT CAUSE: Find the specific error in the artifacts above. Quote the actual error message, status condition, or log line that reveals the failure. Do NOT speculate — cite what you found.\n")
	sb.WriteString("2. TRACE THE CHAIN: Follow the dependency chain (VM provisioning → cloud-init → kubeadm → kubelet → CNI → CCM → providerID). Identify which step failed and why.\n")
	sb.WriteString("3. SUGGESTED FIX: Based on the root cause you identified, give the specific fix. Say exactly what file/config/setting needs to change and how. Do NOT say 'check the logs' — you already have them.\n")
	sb.WriteString("4. If artifacts show the cause clearly, state it with confidence. If evidence is incomplete, say what you determined and what remains unknown.\n\n")
	sb.WriteString(`Respond in JSON: {"root_cause": "the specific error found in evidence with quoted log lines", "severity": "Critical/High/Medium/Low", "suggested_fix": "exact fix with file paths and changes needed", "relevant_files": ["file1.go", "file2.yaml"]}`)

	resp, err := c.callAPI(ctx, DeepModel, systemPrompt, sb.String())
	if err != nil {
		return nil, err
	}

	var parsed deepAnalysisResponse
	cleaned := extractJSON(resp)
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		parsed = deepAnalysisResponse{
			RootCause:    resp,
			Severity:     "Medium",
			SuggestedFix: "Unable to parse structured response",
		}
	}

	analysis := &models.AIAnalysis{
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Model:         DeepModel,
		RootCause:     parsed.RootCause,
		Severity:      parsed.Severity,
		SuggestedFix:  parsed.SuggestedFix,
		RelevantFiles: parsed.RelevantFiles,
	}

	_ = c.cache.Set(key, analysis)
	return analysis, nil
}

// comprehensiveCacheKey builds a deterministic cache key from test name and failure message.
// We intentionally exclude volatile artifact content so cache hits are stable across runs.
func comprehensiveCacheKey(ev Evidence) string {
	h := sha256.New()
	h.Write([]byte(ev.TestName))
	h.Write([]byte(normalizeError(ev.FailureMessage)))
	sum := h.Sum(nil)
	return fmt.Sprintf("comprehensive:%x", sum[:8])
}

// ---------- API helper ----------

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (c *Client) callAPI(ctx context.Context, model, sysPrompt, userMessage string) (string, error) {
	time.Sleep(callDelay)

	reqBody := chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "system", Content: sysPrompt},
			{Role: "user", Content: userMessage},
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Copilot-Integration-Id", "copilot-developer-cli")

	var resp *http.Response
	for attempt := 0; attempt < 3; attempt++ {
		resp, err = c.httpClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("API call failed: %w", err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			wait := time.Duration(2<<attempt) * time.Second
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(wait):
			}
			// Recreate request with fresh body reader.
			req, _ = http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+c.token)
			req.Header.Set("Copilot-Integration-Id", "copilot-developer-cli")
			continue
		}
		break
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API returned %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	var chatResp chatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", fmt.Errorf("parsing response: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("empty response from API")
	}

	return chatResp.Choices[0].Message.Content, nil
}

// ---------- Helpers ----------

// cacheKey builds a deterministic cache key from a prefix and input strings.
func cacheKey(prefix, testName, failureMessage string) string {
	normalized := normalizeError(failureMessage)
	h := sha256.Sum256([]byte(testName + normalized))
	return fmt.Sprintf("%s:%x", prefix, h[:8])
}

var whitespaceRe = regexp.MustCompile(`\s+`)

func normalizeError(msg string) string {
	// Collapse whitespace and remove hex addresses/UUIDs for stable hashing.
	s := whitespaceRe.ReplaceAllString(msg, " ")
	s = regexp.MustCompile(`0x[0-9a-fA-F]+`).ReplaceAllString(s, "<addr>")
	s = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`).ReplaceAllString(s, "<uuid>")
	return strings.TrimSpace(s)
}

// detectTransient checks if the AI summary text indicates a transient failure.
func detectTransient(text string) bool {
	lower := strings.ToLower(text)
	keywords := []string{"transient", "flake", "flaky", "temporary", "throttling", "intermittent", "retry"}
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// extractJSON tries to pull a JSON object from text that may include markdown fences.
func extractJSON(s string) string {
	// Try to find JSON between ```json ... ``` fences.
	re := regexp.MustCompile("(?s)```(?:json)?\\s*({.*?})\\s*```")
	if m := re.FindStringSubmatch(s); len(m) > 1 {
		return m[1]
	}
	// Try to find a bare JSON object.
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}
