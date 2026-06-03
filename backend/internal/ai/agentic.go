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

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// AgenticMode is the value stored in models.AIAnalysis.Mode for results
// produced by the agentic pipeline. The curator pipeline uses "" (legacy)
// or "curator"; both are accepted as "curator" by Service.shouldReanalyze.
const AgenticMode = "agentic"

// UniversalMode is the value stored in models.AIAnalysis.Mode for results
// produced by the use_universal_path flow. Distinct from AgenticMode so
// that flipping a project from agentic-on-top-of-capi to universal
// invalidates previously cached analyses (shouldReanalyze treats any mode
// mismatch as cache-miss).
const UniversalMode = "agentic-universal"

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

	// MinToolCalls is the minimum number of tool calls before a
	// tools-free final answer from the model is accepted as cacheable.
	// Defaults to 0 (no floor; behavior identical to pre-L.3). When set,
	// the loop nudges the model with a "you haven't investigated enough"
	// user message instead of accepting the early final, and skips the
	// cache write for any final that lands below the floor (so the
	// next run gets a fresh attempt). See project.Agentic.MinToolCalls.
	MinToolCalls int
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

type agChatRequest struct {
	Model    string          `json:"model"`
	Messages []agChatMessage `json:"messages"`
	Tools    []tools.Schema  `json:"tools,omitempty"`
}

type agChatResponse struct {
	Choices []struct {
		FinishReason string        `json:"finish_reason"`
		Message      agChatMessage `json:"message"`
	} `json:"choices"`
}

func strPtr(s string) *string { return &s }

// ---------- Tool documentation appended to the system prompt ----------

// agToolDocs is the tool-usage strategy section appended to the system prompt
// by the agentic loop. Tool names + descriptions are already conveyed to the
// model via the schema array in each chat request, so this section focuses on
// budget guidance and high-level investigation strategy rather than restating
// what each tool does.
const agToolDocs = `

## Tool usage strategy

You have a set of tools for browsing the build's GCS artifact tree (see the
tools field of this request for names, descriptions, and parameters).

1. Start by listing the build root to see what's there.
2. For multi-MB build-logs, ALWAYS use grep_artifact, never read_artifact or tail_artifact.
3. Cite actual paths and line numbers in your final answer. Do not speculate; if evidence is incomplete, say so explicitly.
4. Watch the remaining_model_bytes and remaining_gcs_bytes returned with each tool result; stop browsing and produce the final JSON answer before they hit zero.

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

// formatMinToolsNudge builds the user message appended after a tools-free
// model response when the agent has not yet met AgenticOptions.MinToolCalls.
// Kept generic about artifact paths because not every project has a
// build-log.txt at the build root; the universal prompt and tool docs
// already cover specific entry points.
func formatMinToolsNudge(calls, floor int) string {
	return fmt.Sprintf(`You attempted to finalize after only %d tool call(s), but this project requires at least %d before a final answer is accepted. Investigate the build's artifacts before responding: at minimum list the build root, inspect or grep the test's actual failure logs, and cite at least one concrete file path in your root_cause. If after additional investigation the evidence is genuinely inconclusive, say so explicitly in root_cause rather than speculating.`, calls, floor)
}

// agenticCacheData is the on-disk shape of a cached agentic analysis. Embeds
// the raw model response (analysisResponse) and tags it with the per-analysis
// telemetry (tool-call count + cost) so that cache reads can re-stamp the
// published AIAnalysis and re-validate against the project's current
// MinToolCalls floor. Backward compat: pre-L.3 cache entries have no
// telemetry keys and unmarshal with zero values, so any non-zero floor
// invalidates them on read and re-analyzes them on the next run.
type agenticCacheData struct {
	analysisResponse
	ToolCalls       int  `json:"tool_calls,omitempty"`
	ModelBytes      int  `json:"model_bytes,omitempty"`
	GCSBytes        int  `json:"gcs_bytes,omitempty"`
	BudgetExhausted bool `json:"budget_exhausted,omitempty"`
}

// ---------- Agent state ----------

type agentState struct {
	browser         artifacts.Browser
	opts            AgenticOptions
	registry        *tools.Registry
	enabledTools    []string
	cache           *tools.Cache
	webURLBase      string
	startTime       time.Time
	modelBytes      int
	gcsBytes        int
	calls           int
	budgetExhausted bool
}

func (s *agentState) modelRemaining() int { return s.opts.ModelByteBudget - s.modelBytes }
func (s *agentState) gcsRemaining() int   { return s.opts.GCSByteBudget - s.gcsBytes }

// stampTelemetry copies the per-call counters onto the returned AIAnalysis so
// the published JSON exposes per-failure cost. Called at every successful
// agentic exit point (cache hit, normal finish, finalize-round finish,
// synthesized fallback). Mode is set here too so we can't accidentally
// publish an agentic-produced analysis tagged with the curator mode.
//
// An empty mode argument defaults to AgenticMode for back-compat (existing
// call sites pre-L.2 didn't pass one); the universal path passes
// UniversalMode explicitly.
func stampAgenticTelemetry(analysis *models.AIAnalysis, state *agentState, mode string, cacheHit bool, start time.Time) {
	if analysis == nil {
		return
	}
	if mode == "" {
		mode = AgenticMode
	}
	analysis.Mode = mode
	analysis.CacheHit = cacheHit
	analysis.ElapsedMs = int(time.Since(start) / time.Millisecond)
	if state != nil {
		analysis.ToolCalls = state.calls
		analysis.ModelBytes = state.modelBytes
		analysis.GCSBytes = state.gcsBytes
		analysis.BudgetExhausted = state.budgetExhausted
	}
}

// ---------- Public entry point ----------

// AgenticInputs bundles the per-failure context required by the agentic loop.
// Lifetime notes:
//   - Browser, Cache, and WebURLBase are scoped to one build (shared across
//     all failures of that build).
//   - Registry and EnabledTools are scoped to one Service (built once per
//     project at fetcher startup).
//   - Opts is per-project.
//   - Mode is the value to stamp on the returned AIAnalysis. Empty defaults
//     to AgenticMode; UniversalMode flags results produced via the
//     use_universal_path flow so cache invalidation kicks in when consumers
//     flip the switch.
type AgenticInputs struct {
	Browser      artifacts.Browser
	Opts         AgenticOptions
	Registry     *tools.Registry
	EnabledTools []string
	Cache        *tools.Cache
	WebURLBase   string
	Mode         string
}

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
	in AgenticInputs,
	cacheKey, sysPrompt, userPrompt string,
) (*models.AISummary, *models.AIAnalysis, error) {
	start := time.Now()
	if raw, ok := c.cache.Get(cacheKey); ok {
		var cached agenticCacheData
		if json.Unmarshal(raw, &cached) == nil {
			// Cache hit, but re-validate against the current floor. Pre-L.3
			// entries default to ToolCalls=0 and are invalidated by any
			// non-zero MinToolCalls.
			if cached.ToolCalls >= in.Opts.MinToolCalls {
				summary, analysis := c.buildOutputs(cached.analysisResponse)
				stampAgenticTelemetry(analysis, nil, in.Mode, true, start)
				// Restore the recorded per-analysis telemetry from the
				// cache so the published JSON keeps its tool-call /
				// cost / budget-exhausted signals across cache hits.
				// Without this, cache hits would publish ToolCalls=0 and
				// the build-level shouldReanalyze gate would re-trigger
				// reanalysis on every run.
				analysis.ToolCalls = cached.ToolCalls
				analysis.ModelBytes = cached.ModelBytes
				analysis.GCSBytes = cached.GCSBytes
				analysis.BudgetExhausted = cached.BudgetExhausted
				return summary, analysis, nil
			}
		}
	}

	state := &agentState{
		browser:      in.Browser,
		opts:         in.Opts,
		registry:     in.Registry,
		enabledTools: in.EnabledTools,
		cache:        in.Cache,
		webURLBase:   in.WebURLBase,
		startTime:    time.Now(),
	}

	fullSysPrompt := sysPrompt + agToolDocs
	messages := []agChatMessage{
		{Role: "system", Content: strPtr(fullSysPrompt)},
		{Role: "user", Content: strPtr(userPrompt)},
	}
	schemas := state.registry.Schemas(state.enabledTools)

	loopCtx, cancel := context.WithDeadline(ctx, state.startTime.Add(in.Opts.WallClock))
	defer cancel()

	var finalContent string
	var done bool
	// nudgedAtCalls tracks the tool-call count when we last issued a
	// min-tool-calls nudge. We only re-nudge once the model has made new
	// tool-call progress since then; this prevents an infinite no-progress
	// nudge loop when the model genuinely refuses to investigate further.
	nudgedAtCalls := -1

	for iter := 0; iter < in.Opts.MaxIters && !done; iter++ {
		resp, err := c.callChatWithTools(loopCtx, messages, schemas)
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
			candidate := ""
			if msg.Content != nil {
				candidate = *msg.Content
			}

			// Enforce the per-project min-tool-calls floor by nudging the
			// model to investigate further before accepting its final.
			// Skip the nudge when: (a) no floor configured, (b) the model
			// has not made progress since the last nudge (avoid no-progress
			// loops), or (c) budgets are already exhausted (nudge would
			// fight the tool-side "budget exhausted; finalize now" signal).
			if state.calls < in.Opts.MinToolCalls && state.calls > nudgedAtCalls && !state.budgetExhausted {
				echo := agChatMessage{Role: "assistant"}
				if msg.Content != nil {
					echo.Content = msg.Content
				}
				messages = append(messages, echo, agChatMessage{
					Role:    "user",
					Content: strPtr(formatMinToolsNudge(state.calls, in.Opts.MinToolCalls)),
				})
				nudgedAtCalls = state.calls
				log.Printf("  ↻ agentic nudge: tool_calls=%d < min=%d, asking model to investigate further", state.calls, in.Opts.MinToolCalls)
				continue
			}

			finalContent = candidate
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
		stampAgenticTelemetry(analysis, state, in.Mode, false, start)
		return summary, analysis, nil
	}

	c.cacheAcceptedAnalysis(cacheKey, parsed, state, in.Opts)
	summary, analysis := c.buildOutputs(parsed)
	stampAgenticTelemetry(analysis, state, in.Mode, false, start)
	return summary, analysis, nil
}

// cacheAcceptedAnalysis writes a parsed analysis to the cache, but only if the
// agent met the project's MinToolCalls floor. Below-floor finals are still
// published to the dashboard for this run (so triage always has something to
// show) but are NOT cached, so the next fetcher run re-attempts the analysis
// rather than serving the under-investigated answer.
func (c *Client) cacheAcceptedAnalysis(cacheKey string, parsed analysisResponse, state *agentState, opts AgenticOptions) {
	if state.calls < opts.MinToolCalls {
		return
	}
	_ = c.cache.Set(cacheKey, agenticCacheData{
		analysisResponse: parsed,
		ToolCalls:        state.calls,
		ModelBytes:       state.modelBytes,
		GCSBytes:         state.gcsBytes,
		BudgetExhausted:  state.budgetExhausted,
	})
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
func (c *Client) callChatWithTools(ctx context.Context, messages []agChatMessage, toolDefs []tools.Schema) (*agChatResponse, error) {
	time.Sleep(callDelay)

	body, err := json.Marshal(agChatRequest{Model: c.model, Messages: messages, Tools: toolDefs})
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

// dispatchAgenticTool routes one tool call through the registry, accumulates
// bytes/budget telemetry on the agent state, and returns the model-bound
// envelope JSON.
func dispatchAgenticTool(ctx context.Context, s *agentState, tc agToolCall) string {
	s.calls++
	if s.modelRemaining() <= 0 {
		s.budgetExhausted = true
		return toolErrJSON("model byte budget exhausted; produce final JSON now")
	}
	if s.gcsRemaining() <= 0 {
		s.budgetExhausted = true
		return toolErrJSON("GCS byte budget exhausted; produce final JSON now")
	}

	env := &tools.Env{
		Browser:             s.browser,
		Cache:               s.cache,
		WebURLBase:          s.webURLBase,
		RemainingModelBytes: s.modelRemaining(),
		RemainingGCSBytes:   s.gcsRemaining(),
	}
	result := s.registry.Dispatch(ctx, env, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
	s.gcsBytes += result.BytesFetched
	if result.BudgetExhausted {
		s.budgetExhausted = true
	}
	if result.Payload == nil {
		// Defensive: registry promises a non-nil Payload, but never trust the
		// edge case. Empty map is safer than a nil deref in toolEnvelopeJSON.
		result.Payload = map[string]interface{}{}
	}
	return toolEnvelopeJSON(s, result.Payload)
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
