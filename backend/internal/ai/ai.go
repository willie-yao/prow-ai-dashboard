// Package ai provides a generic chat-completion client and the agentic
// tool-calling analysis loop. The per-failure seed prompt is built by the
// universal Module (internal/ai/modules/universal); Module + Client are
// composed by Service, which orchestrates a single test failure's analysis.
package ai

import (
	"context"
	"encoding/json"
	"fmt"
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
	// Model is the default model; override via Options.Model or AI_MODEL env.
	Model = "claude-sonnet-4.5"
)

// callDelay throttles consecutive API calls. var (not const) so tests can
// shrink it; production callers should not touch this.
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

// Options configures a Client. Empty values fall back to the Copilot defaults.
type Options struct {
	Token    string
	CacheDir string
	// Endpoint is the chat-completions URL. Defaults to ModelsAPIURL.
	Endpoint string
	// Model is the model identifier the provider expects. Defaults to Model.
	Model string
	// ExtraHeaders are merged into every request after the defaults. Use
	// this for provider-specific routing headers or to override the default
	// Authorization scheme.
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
	// Clone the default transport so proxy support (ProxyFromEnvironment),
	// TLS, and dial defaults are preserved; only enlarge the idle-connection
	// pool so concurrent analyses (see project.AI.Concurrency) reuse
	// connections to one endpoint instead of churning them.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 32
	transport.MaxIdleConnsPerHost = 16
	return &Client{
		// No client-level Timeout: per-request deadlines come from the
		// caller's context. The agentic loop runs every chat call under the
		// per-failure budget (ai.agentic.timeout, default 5m), and the
		// /v1/models probe sets its own short sub-context. A fixed timeout
		// here would silently override that budget and prematurely kill
		// legitimately slow responses from reasoning/self-hosted endpoints.
		httpClient: &http.Client{
			Transport: transport,
		},
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

// Complete sends a single tool-free chat completion (a system + user message)
// and returns the assistant's text. It is the simple, non-agentic entry point
// for callers that just need a one-shot generation (e.g. onboard's prompt
// drafting). The request is bounded only by ctx.
func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	messages := []agChatMessage{
		{Role: "system", Content: strPtr(system)},
		{Role: "user", Content: strPtr(user)},
	}
	resp, err := c.callChatWithTools(ctx, messages, nil, nil)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == nil {
		return "", fmt.Errorf("empty completion response")
	}
	return *resp.Choices[0].Message.Content, nil
}

// modelsResponse is the subset of the OpenAI-compatible /v1/models payload we
// care about. vLLM / TRT-LLM (Dynamo) report the served model's context window
// here; the field is absent on providers that don't (e.g. Copilot).
type modelsResponse struct {
	Data []struct {
		ID            string `json:"id"`
		ContextWindow int    `json:"context_window"`
	} `json:"data"`
}

// DetectContextWindowTokens queries the endpoint's /v1/models and returns the
// served model's context window in tokens. Returns ok=false (and the caller
// should fall back to defaults) when the endpoint doesn't expose /v1/models,
// doesn't report context_window, or errors. Best-effort: one short GET, no
// retries.
func (c *Client) DetectContextWindowTokens(ctx context.Context) (int, bool) {
	modelsURL, ok := modelsURLFor(c.apiURL)
	if !ok {
		return 0, false
	}
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return 0, false
	}
	c.setRequestHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, false
	}
	var out modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, false
	}
	// Prefer the entry matching the configured model; else the first entry
	// that reports a positive window.
	best := 0
	for _, m := range out.Data {
		if m.ContextWindow <= 0 {
			continue
		}
		if m.ID == c.model {
			return m.ContextWindow, true
		}
		if best == 0 {
			best = m.ContextWindow
		}
	}
	if best > 0 {
		return best, true
	}
	return 0, false
}

// modelsURLFor derives the /v1/models URL from a chat-completions URL by
// swapping the trailing "/chat/completions" for "/models". Returns ok=false
// when the URL doesn't look like a chat-completions endpoint.
func modelsURLFor(chatURL string) (string, bool) {
	const suffix = "/chat/completions"
	base, found := strings.CutSuffix(chatURL, suffix)
	if !found {
		return "", false
	}
	return base + "/models", true
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

// proseFields returns RootCause + Summary + SuggestedFix + RelevantFiles
// for callers that scan across every textual field of the draft.
func (r analysisResponse) proseFields() []string {
	out := make([]string, 0, 3+len(r.RelevantFiles))
	out = append(out, r.RootCause, r.Summary, r.SuggestedFix)
	out = append(out, r.RelevantFiles...)
	return out
}

// buildOutputs splits an analysisResponse into the AISummary + AIAnalysis
// pair the pipeline consumes, both stamped with the same generated_at.
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

// setRequestHeaders applies the standard headers and then merges any
// user-supplied ExtraHeaders. Extras win on conflict so projects can
// override the default Authorization scheme.
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
// API. The Copilot-Integration-Id header is only meaningful there.
func isCopilotEndpoint(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.HasSuffix(u.Hostname(), "githubcopilot.com")
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
