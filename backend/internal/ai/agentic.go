package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// AgenticMode is the value stored in models.AIAnalysis.Mode for results
// produced by the agentic pipeline. The curator pipeline uses "" (legacy)
// or "curator"; both are accepted as "curator" by Service.shouldReanalyze.
const AgenticMode = "agentic"

// ErrToolsUnsupported is returned from the agentic loop when the configured
// provider rejects function-calling on the first call (typically HTTP 400 with
// a body mentioning "tools" or "functions"). Callers should fall back to the
// single-shot curator pipeline for that failure and avoid retrying agentic
// mode against the same endpoint for the rest of the run.
var ErrToolsUnsupported = errors.New("ai endpoint does not support function calling")

// AgenticOptions is the resolved per-failure budget config. Build via
// project.Agentic.EffectiveAgentic() once per fetcher run and reuse.
type AgenticOptions struct {
	MaxIters        int
	ModelByteBudget int
	GCSByteBudget   int
	WallClock       time.Duration
}

// agenticToolBudget is the per-call cap on bytes returned to the model by
// any single tool result. Keeps one runaway response from eating the whole
// ModelByteBudget. 32KB matches the spike, well above any reasonable single
// JSON envelope.
const agenticToolBudget = 32 * 1024

// ---------- Chat protocol (parallel to ai.go's single-shot types) ----------

// agChatMessage uses *string for Content so the tool-call echo can send a
// null content alongside tool_calls, matching the OpenAI spec.
type agChatMessage struct {
	Role       string       `json:"role"`
	Content    *string      `json:"content,omitempty"`
	Name       string       `json:"name,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
	ToolCalls  []agToolCall `json:"tool_calls,omitempty"`
}

type agToolCall struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"`
	Function agFunction `json:"function"`
}

type agFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type agToolDef struct {
	Type     string     `json:"type"`
	Function agToolFunc `json:"function"`
}

type agToolFunc struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type agChatRequest struct {
	Model    string          `json:"model"`
	Messages []agChatMessage `json:"messages"`
	Tools    []agToolDef     `json:"tools,omitempty"`
}

type agChatResponse struct {
	Choices []struct {
		FinishReason string        `json:"finish_reason"`
		Message      agChatMessage `json:"message"`
	} `json:"choices"`
}

func strPtr(s string) *string { return &s }

// ---------- Tool definitions ----------

func agToolDefs() []agToolDef {
	return []agToolDef{
		{
			Type: "function",
			Function: agToolFunc{
				Name:        "list_artifacts",
				Description: "List the immediate children of a directory in the build's GCS artifact tree. Pass an empty string for the build root. Returns dirs and files (with sizes).",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Directory path relative to the build root, e.g. \"\" for root, \"artifacts/\", \"artifacts/clusters/foo/machines/bar/\".",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: agToolFunc{
				Name:        "read_artifact",
				Description: "Read a byte range of a file. Use for small/known files. For large logs prefer tail_artifact or grep_artifact. Returns up to 16384 bytes per call.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path":   map[string]interface{}{"type": "string", "description": "File path relative to build root."},
						"offset": map[string]interface{}{"type": "integer", "description": "Byte offset to start reading from (default 0).", "default": 0},
						"length": map[string]interface{}{"type": "integer", "description": "Number of bytes to read (default 8192, max 16384).", "default": 8192},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: agToolFunc{
				Name:        "tail_artifact",
				Description: "Return the last N lines of a file. Most efficient way to inspect the end of a build log or controller log. Default 500 lines, max 2000.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path":  map[string]interface{}{"type": "string", "description": "File path relative to build root."},
						"lines": map[string]interface{}{"type": "integer", "description": "Number of trailing lines (default 500, max 2000).", "default": 500},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: agToolFunc{
				Name:        "grep_artifact",
				Description: "Regex-search a file for matching lines. Returns matches with surrounding context lines and line numbers. Use this for huge build-logs where you want to find specific errors.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path":          map[string]interface{}{"type": "string", "description": "File path relative to build root."},
						"pattern":       map[string]interface{}{"type": "string", "description": "RE2 regex (Go syntax). Use (?i) prefix for case-insensitive."},
						"context_lines": map[string]interface{}{"type": "integer", "description": "Lines of context before/after each match (default 2, max 5).", "default": 2},
						"max_matches":   map[string]interface{}{"type": "integer", "description": "Max matches to return (default 30, max 100).", "default": 30},
					},
					"required": []string{"path", "pattern"},
				},
			},
		},
	}
}

// agToolDocs is the tool-usage section appended to the system prompt by the
// agentic loop. Kept separate from the consumer's prompts/system.md so that
// the engine can evolve the tool surface without forcing every consumer to
// edit their prompt.
const agToolDocs = `

## Available tools

You have four tools for browsing the build's GCS artifact tree:
  list_artifacts(path)                    - list immediate children of a dir
  read_artifact(path, offset, length)     - read byte range of a file (max 16KB)
  tail_artifact(path, lines)              - last N lines of a file (max 2000)
  grep_artifact(path, pattern, ...)       - RE2 regex search with line numbers

Strategy:
1. Start by listing the build root to see what's there.
2. Drill into artifacts/clusters/<name>/ for per-cluster dumps and per-machine logs.
3. For multi-MB build-logs, ALWAYS use grep_artifact, never read_artifact or tail_artifact.
4. Cite actual paths and line numbers in your final answer. Do not speculate; if evidence is incomplete, say so explicitly.
5. Watch the remaining_model_bytes and remaining_gcs_bytes returned with each tool result; stop browsing and produce the final JSON answer before they hit zero.

Tool calls cost time. Prefer 3-5 focused calls over many small ones. A confident "best evidence I found" answer beats running out of budget mid-investigation.`

// agForceFinalizePrompt is the user message used to force a JSON-only final
// round when the model has either exhausted iterations or returned text
// without valid JSON.
const agForceFinalizePrompt = `Stop calling tools. Produce the final JSON
analysis now using the evidence you have already gathered, following the
"Response format" section of the system prompt exactly. If you did not find a
definitive root cause, say so explicitly in root_cause (e.g. "Investigation
reached budget; best-evidence hypothesis is X based on Y") rather than
continuing to investigate.`

// ---------- Agent state ----------

type agentState struct {
	browser    artifacts.Browser
	opts       AgenticOptions
	startTime  time.Time
	modelBytes int
	gcsBytes   int
	calls      int
}

func (s *agentState) modelRemaining() int { return s.opts.ModelByteBudget - s.modelBytes }
func (s *agentState) gcsRemaining() int   { return s.opts.GCSByteBudget - s.gcsBytes }

// ---------- Public entry point ----------

// doAnalyzeAgentic runs the tool-calling AI loop for one failure. Returns the
// same (summary, analysis) pair as doAnalyze so callers can treat both
// pipelines uniformly.
//
// The caller is responsible for constructing a fresh Browser per failure
// (typically via artifacts.Factory.ForBuild) and for choosing the cache key
// (which MUST encode build+failure so two builds of the same test never share
// an agentic cache entry).
//
// Returns ErrToolsUnsupported wrapped on the first API call if the endpoint
// rejects function-calling. The caller should fall back to doAnalyze for that
// failure.
func (c *Client) doAnalyzeAgentic(
	ctx context.Context,
	browser artifacts.Browser,
	opts AgenticOptions,
	cacheKey, sysPrompt, userPrompt string,
) (*models.AISummary, *models.AIAnalysis, error) {
	if raw, ok := c.cache.Get(cacheKey); ok {
		var parsed analysisResponse
		if json.Unmarshal(raw, &parsed) == nil {
			summary, analysis := c.buildOutputs(parsed)
			if analysis != nil {
				analysis.Mode = AgenticMode
			}
			return summary, analysis, nil
		}
	}

	state := &agentState{
		browser:   browser,
		opts:      opts,
		startTime: time.Now(),
	}

	fullSysPrompt := sysPrompt + agToolDocs
	messages := []agChatMessage{
		{Role: "system", Content: strPtr(fullSysPrompt)},
		{Role: "user", Content: strPtr(userPrompt)},
	}
	tools := agToolDefs()

	loopCtx, cancel := context.WithDeadline(ctx, state.startTime.Add(opts.WallClock))
	defer cancel()

	var finalContent string
	var done bool

	for iter := 0; iter < opts.MaxIters && !done; iter++ {
		resp, err := c.callChatWithTools(loopCtx, messages, tools)
		if err != nil {
			// Detect "tools not supported" on the first call only.
			if iter == 0 && isToolsUnsupportedError(err) {
				return nil, nil, fmt.Errorf("%w: %v", ErrToolsUnsupported, err)
			}
			return nil, nil, fmt.Errorf("agentic iter %d: %w", iter+1, err)
		}
		if len(resp.Choices) == 0 {
			return nil, nil, fmt.Errorf("agentic iter %d: empty choices", iter+1)
		}
		choice := resp.Choices[0]
		msg := choice.Message

		if len(msg.ToolCalls) == 0 {
			if msg.Content != nil {
				finalContent = *msg.Content
			}
			done = true
			break
		}

		echo := agChatMessage{Role: "assistant", ToolCalls: msg.ToolCalls}
		if msg.Content != nil {
			echo.Content = msg.Content
		}
		messages = append(messages, echo)

		for _, tc := range msg.ToolCalls {
			result := dispatchAgenticTool(loopCtx, state, tc)
			state.modelBytes += len(result)
			messages = append(messages, agChatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    strPtr(result),
			})
		}
	}

	// If the model never returned a tools-free final message, OR returned one
	// without parseable JSON, force a finalize round with tools omitted.
	parsed, ok := tryParseAnalysis(finalContent)
	if !ok {
		finalContent = c.runFinalizeRound(loopCtx, messages)
		parsed, ok = tryParseAnalysis(finalContent)
	}
	if !ok {
		// Last resort: synthesize an analysisResponse from the raw text so the
		// UI still has something to render. Do NOT cache this — a transient
		// model glitch shouldn't permanently poison the cache.
		parsed = analysisResponse{
			Summary:      firstSentence(finalContent),
			RootCause:    finalContent,
			Severity:     "Medium",
			SuggestedFix: "Unable to parse structured response",
		}
		summary, analysis := c.buildOutputs(parsed)
		if analysis != nil {
			analysis.Mode = AgenticMode
		}
		return summary, analysis, nil
	}

	_ = c.cache.Set(cacheKey, parsed)
	summary, analysis := c.buildOutputs(parsed)
	if analysis != nil {
		analysis.Mode = AgenticMode
	}
	return summary, analysis, nil
}

// runFinalizeRound asks the model for one more no-tools response containing
// just the final JSON. Used when the agent ran out of iterations or returned
// prose without parseable JSON. Returns the raw content (which may itself be
// unparseable; callers handle that).
func (c *Client) runFinalizeRound(ctx context.Context, messages []agChatMessage) string {
	messages = append(messages, agChatMessage{Role: "user", Content: strPtr(agForceFinalizePrompt)})
	resp, err := c.callChatWithTools(ctx, messages, nil)
	if err != nil {
		log.Printf("  ⚠ agentic finalize round failed: %v", err)
		return ""
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == nil {
		return ""
	}
	return *resp.Choices[0].Message.Content
}

// tryParseAnalysis extracts and unmarshals the JSON answer, returning ok=false
// if no valid JSON object could be found.
func tryParseAnalysis(s string) (analysisResponse, bool) {
	if strings.TrimSpace(s) == "" {
		return analysisResponse{}, false
	}
	var out analysisResponse
	cleaned := extractJSON(s)
	if err := json.Unmarshal([]byte(cleaned), &out); err != nil {
		return analysisResponse{}, false
	}
	if out.RootCause == "" && out.Summary == "" {
		return analysisResponse{}, false
	}
	return out, true
}

// ---------- HTTP call ----------

var toolsUnsupportedRe = regexp.MustCompile(`(?i)tool[s_]?call|function[s_]?call|tools_choice|tools provided|function calling`)

func isToolsUnsupportedError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, " 400") && !strings.Contains(msg, " 422") {
		return false
	}
	return toolsUnsupportedRe.MatchString(msg)
}

// callChatWithTools sends a chat-completions request with optional tool defs
// and parses the OpenAI-shaped response. Retries on 429 like the single-shot
// path. Sleeps the same callDelay between calls to be a good citizen.
func (c *Client) callChatWithTools(ctx context.Context, messages []agChatMessage, tools []agToolDef) (*agChatResponse, error) {
	time.Sleep(callDelay)

	body, err := json.Marshal(agChatRequest{Model: c.model, Messages: messages, Tools: tools})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	var resp *http.Response
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		c.setRequestHeaders(req)
		resp, err = c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("post: %w", err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			wait := time.Duration(2<<attempt) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		break
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("chat returned %d: %s", resp.StatusCode, truncate(string(rb), 500))
	}
	var out agChatResponse
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w; body=%s", err, truncate(string(rb), 500))
	}
	return &out, nil
}

// ---------- Tool dispatch ----------

func dispatchAgenticTool(ctx context.Context, s *agentState, tc agToolCall) string {
	s.calls++
	if s.modelRemaining() <= 0 {
		return toolErrJSON("model byte budget exhausted; produce final JSON now")
	}
	if s.gcsRemaining() <= 0 {
		return toolErrJSON("GCS byte budget exhausted; produce final JSON now")
	}

	var args struct {
		Path         string `json:"path"`
		Offset       int    `json:"offset"`
		Length       int    `json:"length"`
		Lines        int    `json:"lines"`
		Pattern      string `json:"pattern"`
		ContextLines int    `json:"context_lines"`
		MaxMatches   int    `json:"max_matches"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return toolErrJSON(fmt.Sprintf("invalid arguments: %v", err))
	}

	switch tc.Function.Name {
	case "list_artifacts":
		return doList(ctx, s, args.Path)
	case "read_artifact":
		if args.Length <= 0 {
			args.Length = 8192
		}
		if args.Length > 16384 {
			args.Length = 16384
		}
		return doRead(ctx, s, args.Path, args.Offset, args.Length)
	case "tail_artifact":
		if args.Lines <= 0 {
			args.Lines = 500
		}
		if args.Lines > 2000 {
			args.Lines = 2000
		}
		return doTail(ctx, s, args.Path, args.Lines)
	case "grep_artifact":
		if args.ContextLines < 0 {
			args.ContextLines = 0
		}
		if args.ContextLines > 5 {
			args.ContextLines = 5
		}
		if args.MaxMatches <= 0 {
			args.MaxMatches = 30
		}
		if args.MaxMatches > 100 {
			args.MaxMatches = 100
		}
		return doGrep(ctx, s, args.Path, args.Pattern, args.ContextLines, args.MaxMatches)
	default:
		return toolErrJSON("unknown tool: " + tc.Function.Name)
	}
}

func toolEnvelopeJSON(s *agentState, payload map[string]interface{}) string {
	payload["remaining_model_bytes"] = s.modelRemaining()
	payload["remaining_gcs_bytes"] = s.gcsRemaining()
	payload["elapsed_seconds"] = int(time.Since(s.startTime).Seconds())
	out, _ := json.Marshal(payload)
	return capJSON(string(out))
}

func toolErrJSON(msg string) string {
	out, _ := json.Marshal(map[string]string{"error": msg})
	return string(out)
}

// capJSON trims a tool result to agenticToolBudget so a single response can't
// blow the per-call budget. Returned as-is when within budget.
func capJSON(s string) string {
	if len(s) <= agenticToolBudget {
		return s
	}
	return s[:agenticToolBudget] + `..."truncated":true}`
}

func doList(ctx context.Context, s *agentState, p string) string {
	res, err := s.browser.List(ctx, p)
	if err != nil {
		return toolErrJSON(err.Error())
	}
	payload := map[string]interface{}{
		"dir":  res.Dir,
		"dirs": res.Dirs,
	}
	files := make([]map[string]interface{}, 0, len(res.Files))
	for _, f := range res.Files {
		files = append(files, map[string]interface{}{"name": f.Name, "size": f.Size})
	}
	payload["files"] = files
	if res.Truncated {
		payload["truncated"] = true
	}
	return toolEnvelopeJSON(s, payload)
}

func doRead(ctx context.Context, s *agentState, p string, offset, length int) string {
	data, size, err := s.browser.Read(ctx, p, offset, length)
	if err != nil {
		return toolErrJSON(err.Error())
	}
	s.gcsBytes += len(data)
	return toolEnvelopeJSON(s, map[string]interface{}{
		"path":      p,
		"file_size": size,
		"offset":    offset,
		"length":    len(data),
		"content":   string(data),
	})
}

func doTail(ctx context.Context, s *agentState, p string, lines int) string {
	// Cap suffix-range fetch at 8x line budget * 200 chars per line, well
	// within agenticToolBudget after envelope overhead.
	maxBytes := agenticToolBudget - 256
	if maxBytes < 4096 {
		maxBytes = 4096
	}
	res, err := s.browser.Tail(ctx, p, lines, maxBytes)
	if err != nil {
		return toolErrJSON(err.Error())
	}
	s.gcsBytes += len(res.Content)
	return toolEnvelopeJSON(s, map[string]interface{}{
		"path":           p,
		"file_size":      res.FileSize,
		"lines_returned": res.LinesReturned,
		"content":        string(res.Content),
	})
}

func doGrep(ctx context.Context, s *agentState, p, pattern string, contextLines, maxMatches int) string {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return toolErrJSON("invalid regex: " + err.Error())
	}
	res, err := s.browser.Grep(ctx, p, re, contextLines, maxMatches, 1000)
	if err != nil {
		return toolErrJSON(err.Error())
	}
	s.gcsBytes += int(res.BytesScanned)
	matches := make([]map[string]interface{}, 0, len(res.Matches))
	for _, m := range res.Matches {
		matches = append(matches, map[string]interface{}{
			"line":    m.LineNo,
			"context": m.Context,
		})
	}
	return toolEnvelopeJSON(s, map[string]interface{}{
		"path":          p,
		"file_size":     res.FileSize,
		"total_matches": res.TotalMatches,
		"matches":       matches,
		"truncated":     res.Truncated,
	})
}
