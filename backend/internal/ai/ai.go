// Package ai provides a generic chat-completion client with a project-pluggable
// Module abstraction. Project-specific prompts and evidence collection live in
// internal/ai/modules/<id>/. Module + Client are composed by Service, which
// orchestrates a single test failure's analysis.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
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

// ---------- Low-level API calls ----------

// quickSummaryResponse is the expected JSON structure from the quick model.
type quickSummaryResponse struct {
	Summary     string `json:"summary"`
	IsTransient bool   `json:"is_transient"`
}

// deepAnalysisResponse is the expected JSON structure from the deep model.
type deepAnalysisResponse struct {
	RootCause     string   `json:"root_cause"`
	Severity      string   `json:"severity"`
	SuggestedFix  string   `json:"suggested_fix"`
	RelevantFiles []string `json:"relevant_files"`
}

// doQuickSummary runs a quick chat completion, parses the JSON response into an
// AISummary, and caches the result. Returns the cached value on a hit.
func (c *Client) doQuickSummary(ctx context.Context, cacheKey, sysPrompt, userPrompt string) (*models.AISummary, error) {
	if raw, ok := c.cache.Get(cacheKey); ok {
		var s models.AISummary
		if json.Unmarshal(raw, &s) == nil {
			return &s, nil
		}
	}

	resp, err := c.callAPI(ctx, DefaultModel, sysPrompt, userPrompt)
	if err != nil {
		return nil, err
	}

	var parsed quickSummaryResponse
	cleaned := extractJSON(resp)
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		// Fallback: use raw text, assume not transient.
		parsed = quickSummaryResponse{Summary: resp, IsTransient: false}
	}

	summary := &models.AISummary{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Summary:     parsed.Summary,
		IsTransient: parsed.IsTransient,
	}

	_ = c.cache.Set(cacheKey, summary)
	return summary, nil
}

// doDeepAnalysis runs a deep chat completion, parses the JSON response into an
// AIAnalysis, and caches the result. Returns the cached value on a hit.
func (c *Client) doDeepAnalysis(ctx context.Context, cacheKey, sysPrompt, userPrompt string) (*models.AIAnalysis, error) {
	if raw, ok := c.cache.Get(cacheKey); ok {
		var a models.AIAnalysis
		if json.Unmarshal(raw, &a) == nil {
			return &a, nil
		}
	}

	resp, err := c.callAPI(ctx, DeepModel, sysPrompt, userPrompt)
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

	_ = c.cache.Set(cacheKey, analysis)
	return analysis, nil
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
