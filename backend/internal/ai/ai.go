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
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

const (
	// ModelsAPIURL is the GitHub Copilot chat completions endpoint.
	ModelsAPIURL = "https://api.githubcopilot.com/chat/completions"
	// Model is used for the combined summary + root-cause analysis.
	// claude-opus-4.7-xhigh: extra-high reasoning effort, 200K context.
	// Marked "(Internal only)" in the Copilot agent model list but currently
	// accepted by api.githubcopilot.com; treat availability as best-effort
	// and override via project.yaml or AI_MODEL when a token lacks access.
	Model = "claude-opus-4.7-xhigh"
)

// callDelay throttles consecutive API calls to be polite to upstream rate
// limits. var (not const) so tests can shrink it; production callers should
// not touch this.
var callDelay = 500 * time.Millisecond

// Client calls an OpenAI chat-completions compatible API for AI analysis.
type Client struct {
	httpClient   *http.Client
	apiURL       string
	token        string
	model        string
	extraHeaders map[string]string
	cache        *Cache
}

// Options configures a Client. All fields are optional; empty values fall back
// to the GitHub Copilot defaults (ModelsAPIURL and Model). Token and CacheDir
// are required for a useful client; the caller is responsible for supplying
// them from the environment / project config.
type Options struct {
	Token    string
	CacheDir string
	// Endpoint is the chat-completions URL. Defaults to ModelsAPIURL
	// (https://api.githubcopilot.com/chat/completions).
	Endpoint string
	// Model is the model identifier the provider expects. Defaults to Model
	// (claude-opus-4.7-xhigh) which only the Copilot endpoint accepts;
	// override when pointing at any other provider.
	Model string
	// ExtraHeaders are merged into every request after the defaults. Use this
	// to add provider-specific routing headers (e.g. NIM-Function-Id for
	// Nvidia Dynamo) or to override the default Authorization scheme.
	ExtraHeaders map[string]string
}

// NewClient creates a Client using the Copilot defaults. Kept for callers
// that don't need the new provider knobs.
func NewClient(token string, cacheDir string) *Client {
	return NewClientWithOptions(Options{Token: token, CacheDir: cacheDir})
}

// NewClientWithOptions creates a Client from explicit options, applying
// Copilot defaults for any empty fields.
func NewClientWithOptions(opts Options) *Client {
	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = ModelsAPIURL
	}
	model := opts.Model
	if model == "" {
		model = Model
	}
	return &Client{
		httpClient:   &http.Client{Timeout: 60 * time.Second},
		apiURL:       endpoint,
		token:        opts.Token,
		model:        model,
		extraHeaders: opts.ExtraHeaders,
		cache:        NewCache(opts.CacheDir),
	}
}

// Endpoint returns the configured chat-completions URL (mainly for logging).
func (c *Client) Endpoint() string { return c.apiURL }

// ModelName returns the configured model identifier (mainly for logging).
func (c *Client) ModelName() string { return c.model }

// Cache returns the underlying cache so callers can persist it.
func (c *Client) Cache() *Cache {
	return c.cache
}

// ---------- Low-level API calls ----------

// analysisResponse is the expected JSON structure from the analysis model.
// Combines the headline summary, transient classification, and deep root-cause
// fields in a single response so the list view and detail view always agree.
type analysisResponse struct {
	Summary       string   `json:"summary"`
	IsTransient   bool     `json:"is_transient"`
	RootCause     string   `json:"root_cause"`
	Severity      string   `json:"severity"`
	SuggestedFix  string   `json:"suggested_fix"`
	RelevantFiles []string `json:"relevant_files"`
}

// doAnalyze runs a single chat completion, parses the JSON response, and caches
// it. Returns both the brief AISummary (for the list view) and the deep
// AIAnalysis (for the detail page), derived from the same model output.
func (c *Client) doAnalyze(ctx context.Context, cacheKey, sysPrompt, userPrompt string) (*models.AISummary, *models.AIAnalysis, error) {
	if raw, ok := c.cache.Get(cacheKey); ok {
		var parsed analysisResponse
		if json.Unmarshal(raw, &parsed) == nil {
			summary, analysis := c.buildOutputs(parsed)
			return summary, analysis, nil
		}
	}

	resp, err := c.callAPI(ctx, c.model, sysPrompt, userPrompt)
	if err != nil {
		return nil, nil, err
	}

	var parsed analysisResponse
	cleaned := extractJSON(resp)
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		// Fallback: treat the whole response as the root cause.
		parsed = analysisResponse{
			Summary:      truncate(resp, 200),
			RootCause:    resp,
			Severity:     "Medium",
			SuggestedFix: "Unable to parse structured response",
		}
	}

	// Persist the parsed struct so future reads always get a consistent shape.
	_ = c.cache.Set(cacheKey, parsed)

	summary, analysis := c.buildOutputs(parsed)
	return summary, analysis, nil
}

// buildOutputs splits an analysisResponse into the two structs the rest of the
// pipeline consumes. The same generated_at timestamp is stamped on both so the
// UI can correlate them.
func (c *Client) buildOutputs(parsed analysisResponse) (*models.AISummary, *models.AIAnalysis) {
	now := time.Now().UTC().Format(time.RFC3339)

	summaryText := parsed.Summary
	if summaryText == "" {
		summaryText = firstSentence(parsed.RootCause)
	}

	summary := &models.AISummary{
		GeneratedAt: now,
		Summary:     summaryText,
		IsTransient: parsed.IsTransient,
	}
	analysis := &models.AIAnalysis{
		GeneratedAt:   now,
		Model:         c.model,
		Mode:          curatorMode,
		RootCause:     parsed.RootCause,
		Severity:      parsed.Severity,
		SuggestedFix:  parsed.SuggestedFix,
		RelevantFiles: parsed.RelevantFiles,
	}
	return summary, analysis
}

// firstSentence returns the first sentence (or first 200 chars) of s, used to
// derive a list-view summary when the model omits an explicit "summary" field.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for i, r := range s {
		if r == '.' || r == '\n' {
			return strings.TrimSpace(s[:i+1])
		}
		if i >= 200 {
			break
		}
	}
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
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

// setRequestHeaders applies the standard headers (Content-Type, bearer auth,
// conditional Copilot routing header) and then merges the user-supplied
// ExtraHeaders. Extras win on conflict so projects can override the auth
// scheme (e.g. Azure OpenAI's "api-key") if needed.
func (c *Client) setRequestHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if isCopilotEndpoint(c.apiURL) {
		req.Header.Set("Copilot-Integration-Id", "copilot-developer-cli")
	}
	for k, v := range c.extraHeaders {
		req.Header.Set(k, v)
	}
}

// isCopilotEndpoint reports whether the URL points at GitHub Copilot's models
// API. The Copilot-Integration-Id header is only meaningful there; sending it
// to other providers (Dynamo, OpenAI, Azure) is harmless noise at best.
func isCopilotEndpoint(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.HasSuffix(u.Hostname(), "githubcopilot.com")
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
	c.setRequestHeaders(req)

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
			req, _ = http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL, bytes.NewReader(body))
			c.setRequestHeaders(req)
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
