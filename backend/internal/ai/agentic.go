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
	"path"
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

	// MinGCSBytes is the minimum cumulative GCS bytes fetched via tool
	// calls before a tools-free final answer is accepted. Complements
	// MinToolCalls because tool-call count alone is gameable: weaker
	// models can satisfy a calls floor with cheap list calls or tiny
	// reads and still finalize without evidence (observed 6 calls
	// returning 13 KB total against Qwen3-235B). Defaults to 0 (no
	// floor). See project.Agentic.MinGCSBytes.
	MinGCSBytes int

	// CritiqueEnabled opts the run into the L.4 Step 2 regex critique:
	// after the agentic loop produces a parseable tools-free final,
	// run critiqueDraft on it. If it punts (suggested_fix is a
	// diagnostic/information-gathering TODO list instead of a concrete
	// remediation), append targeted feedback and re-prompt up to
	// CritiqueMaxRetries times. Drafts that still punt after retries
	// are published but not cached, so the next run gets a fresh
	// attempt (mirrors the MinToolCalls / MinGCSBytes anti-thrash
	// pattern). Defaults to false; behavior identical to pre-L.4-Step-2
	// when off. See project.Agentic.Critique.
	CritiqueEnabled bool

	// CritiqueMaxRetries caps how many extra re-prompt rounds the
	// loop will spend on critique. 0 means "critique once but never
	// retry" (acts as a pure don't-cache gate); 2 means the model
	// gets the original final plus up to 2 retries (so 3 total
	// critique evaluations per analysis). Each retry consumes one
	// extra agentic iteration. Only meaningful when CritiqueEnabled.
	CritiqueMaxRetries int
}

// agenticToolBudget is the per-call cap on bytes returned to the model by
// any single tool result. Keeps one runaway response from eating the whole
// ModelByteBudget. 32KB matches the spike, well above any reasonable single
// JSON envelope.
const agenticToolBudget = 32 * 1024

// critiqueRetryIters is the per-retry budget granted to the agentic loop
// when a critique re-prompt is appended. Generous enough to cover the
// expected response shape (1-2 follow-up tool calls + the new tools-free
// final) plus a little slack. Tighter values (e.g. 1) starve retries
// where the model elects to do more investigation before re-emitting —
// caught by the L.4 Step 2 rubber-duck review.
const critiqueRetryIters = 3

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
// model via the schema array in each chat request, so this section focuses
// on investigation strategy: drill into specifics, don't punt to the user,
// and stop only when evidence is genuinely exhausted (not when the first
// plausible symptom is found).
const agToolDocs = `

## Tool usage strategy

You have a set of tools for browsing the build's GCS artifact tree (see the
tools field of this request for names, descriptions, and parameters).

1. Start by listing the build root to see what's there.
2. For multi-MB build-logs, ALWAYS use grep_artifact (with wide surrounding context, e.g. ctx=20), never read_artifact or tail_artifact.
3. Drill into the most relevant named resources. If your current best causal lead depends on a specific resource (a failing Machine, Pod, Node, VM, container, controller, or owning workload), read that resource's own artifacts before finalizing. Do not chase every resource name mentioned in passing; pick the 1-3 most directly tied to the failure. Examples: a failing resource X → read its manifest/status conditions, events, owner-controller log filtered for "X", and any resource-specific runtime logs. For CAPI/CAPZ jobs this typically means AzureMachine/X.yaml + cloud-init/kubelet/journal logs for that machine + the controller-manager log filtered for "X". Stopping at the first plausible symptom is the most common failure mode of this tool; treat each symptom as a lead, not the answer.
4. Investigation is YOUR job, not the user's. suggested_fix must be a concrete remediation action (a code change, config edit, command to run, retry, redeploy, rollback, operational fix). It must NOT be a diagnostic or information-gathering task. If the sentence's primary purpose is to learn more (check, verify, investigate, ensure, inspect, examine, confirm, audit, review, look into, determine), it belongs in your tool work BEFORE finalizing, not in suggested_fix. A "then validate by ..." clause is fine, but only after a concrete remediation. If after following the directly relevant artifact leads you still cannot identify a concrete remediation, say so explicitly in suggested_fix and include all three of: (a) the strongest fact you established, (b) the specific artifacts/logs you consulted, (c) the exact missing evidence that prevents a remediation. Do not invoke this escape hatch if any standard remediation or best-evidence operational action is supported by the artifacts you read.
5. Cite actual paths and quoted log lines in your final answer. Do not speculate; if evidence is incomplete, state what is known and what remains unclear.
6. Watch the remaining_model_bytes and remaining_gcs_bytes returned with each tool result; stop browsing and produce the final JSON answer before they hit zero.

Before finalizing, self-check:
- Did I identify the earliest upstream cause, not just the terminal symptom?
- Did I read the artifacts for the 1-3 named resources most central to the failure?
- Is suggested_fix a remediation action, not a request for more investigation?

A confident "I found X by reading Y at line Z" answer always beats "you should check X". The difference between a useful diagnosis and a useless one is whether the agent did the drilling itself or passed the work back to the user.`

// agForceFinalizePrompt is the user message used to force a JSON-only final
// round when the model has either exhausted iterations or returned text
// without valid JSON.
const agForceFinalizePrompt = `Stop calling tools. Produce the final JSON
analysis now using the evidence you have already gathered, following the
"Response format" section of the system prompt exactly. If you did not find a
definitive root cause, say so explicitly in root_cause (e.g. "Investigation
reached budget; best-evidence hypothesis is X based on Y") rather than
continuing to investigate.`

// formatFloorsNudge builds the user message appended after a tools-free
// model response when one or both per-project floors are unmet. Mentions
// only the axes that are actually unmet so a project that only configures
// MinToolCalls doesn't see a misleading "0 KB" complaint, and vice versa.
//
// The operational guidance below is general enough to apply to any prow
// project (k8s-sigs CAPI providers, kubelet sig-node, etc.); examples of
// "downstream symptoms" are flagged as examples rather than rules to
// avoid over-anchoring the model on the CAPZ cases that motivated this.
func formatFloorsNudge(state *agentState, opts AgenticOptions) string {
	var unmet []string
	if state.calls < opts.MinToolCalls {
		unmet = append(unmet, fmt.Sprintf("only %d tool call(s) but need at least %d", state.calls, opts.MinToolCalls))
	}
	if state.gcsBytes < opts.MinGCSBytes {
		unmet = append(unmet, fmt.Sprintf("only %d KB of GCS evidence but need at least %d KB", state.gcsBytes/1024, opts.MinGCSBytes/1024))
	}
	return fmt.Sprintf(`You attempted to finalize after %s, which this project requires before a final answer is accepted. Before responding:

1. List the build root with list_artifacts to see what's actually there.
2. For multi-MB build logs, use grep_artifact (not read_artifact) and ask for many matches with wide surrounding context so you see chains of causation, not isolated lines.
3. When build-log.txt shows an error, cross-reference the corresponding timestamp in the relevant controller manager.log under artifacts/clusters/.../<namespace>/<deployment>/ or equivalent. Symptoms surfaced in build-log are often downstream of root causes in the controller.
4. Don't accept the first plausible explanation. Common terminal symptoms (for example kubelet/API-server timeouts, context deadline exceeded, NotReady nodes) usually have earlier upstream causes such as webhook/cert problems, leader-election loss, image pull failures, or missing dependencies. Search nearby logs before concluding.
5. Cite specific file paths and log line numbers in your root_cause. Include enough evidence to explain the causal chain, not just the surface error.

If after this investigation the evidence is genuinely inconclusive, say so explicitly in root_cause rather than speculating.`, strings.Join(unmet, " and "))
}

// agenticCacheData is the on-disk shape of a cached agentic analysis. Embeds
// the raw model response (analysisResponse) and tags it with the per-analysis
// telemetry (tool-call count + cost) so that cache reads can re-stamp the
// published AIAnalysis and re-validate against the project's current
// MinToolCalls / MinGCSBytes floors. Backward compat: pre-L.3 cache entries
// have no telemetry keys and unmarshal with zero values, so any non-zero
// floor invalidates them on read and re-analyzes them on the next run.
type agenticCacheData struct {
	analysisResponse
	ToolCalls       int  `json:"tool_calls,omitempty"`
	ModelBytes      int  `json:"model_bytes,omitempty"`
	GCSBytes        int  `json:"gcs_bytes,omitempty"`
	BudgetExhausted bool `json:"budget_exhausted,omitempty"`

	// CritiquePassed is set only when the L.4 Step 2 critique gate
	// ran against this draft and passed. Defaults to false on cache
	// reads of pre-Step-2 entries (no critique ran) and on entries
	// written while critique was disabled. Used by the cache-read
	// gate to invalidate uncritiqued entries when a consumer enables
	// critique (mirrors the L.3 raised-floor invalidation pattern).
	CritiquePassed bool `json:"critique_passed,omitempty"`

	// CritiqueVersion records which contract version this draft passed
	// critique under. L.4 Step 2 wrote no version (deserializes as 0);
	// L.4 Step 2.5 sets currentCritiqueVersion=2. The cache-read gate
	// requires the cached version to be at least the current version
	// when critique is enabled, so strengthening the gate (e.g. adding
	// the hallucination check) properly invalidates entries that
	// passed under the weaker contract. Bumped only on material
	// strengthenings to keep cache churn proportional.
	CritiqueVersion int `json:"critique_version,omitempty"`
}

// floorStatus tracks which per-project floors are currently unmet for a
// given agent state. Used by both the loop's nudge decision and the
// nudge message composer so the two stay in sync.
type floorStatus struct {
	callsUnmet bool
	gcsUnmet   bool
}

func (fs floorStatus) anyUnmet() bool { return fs.callsUnmet || fs.gcsUnmet }

// evalFloors returns which of the per-project floors the current agent
// state fails to meet. A floor configured as 0 (the default) is never
// reported as unmet, preserving pre-L.3 behavior for consumers that
// don't opt in.
func evalFloors(state *agentState, opts AgenticOptions) floorStatus {
	return floorStatus{
		callsUnmet: state.calls < opts.MinToolCalls,
		gcsUnmet:   state.gcsBytes < opts.MinGCSBytes,
	}
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

	// critiquePassed records whether the final accepted answer cleared
	// the L.4 Step 2 critique gate. Stamped onto the published
	// AIAnalysis so the build-level shouldReanalyze gate can invalidate
	// uncritiqued entries when a consumer later enables critique.
	// Meaningful only when opts.CritiqueEnabled.
	critiquePassed bool

	// readArtifactsFull / readArtifactsBase track which artifacts the
	// agent has successfully fetched via read_artifact / tail_artifact
	// / grep_artifact. Used by the L.4 Step 2.5 hallucination check in
	// critiqueDraft to flag prose citations of files the agent never
	// actually opened. Keys are lowercased, slash-normalized; "full"
	// keeps the directory prefix (catches cross-machine basename
	// collisions where two clusters both have a boot.log), "base" is
	// just path.Base (matches the model's bare-basename citations).
	// Populated only after a successful tool dispatch (no error in the
	// returned payload) so failed reads don't silently satisfy the
	// gate. Both maps stay nil until first use to keep the no-critique
	// path zero-allocation.
	readArtifactsFull map[string]bool
	readArtifactsBase map[string]bool
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
		analysis.CritiquePassed = state.critiquePassed
		if state.critiquePassed {
			analysis.CritiqueVersion = currentCritiqueVersion
		}
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
			// Cache hit, but re-validate against the current floors.
			// Pre-L.3 entries default to ToolCalls=0 and GCSBytes=0 and
			// are invalidated by any non-zero floor. Post-L.3 entries
			// must satisfy whichever floors the project currently
			// configures (raising either field invalidates entries
			// that fell below it on a prior run).
			//
			// L.4 Step 2: also invalidate entries that weren't
			// critique-checked when critique is now enabled. Pre-Step-2
			// entries and entries written while critique was disabled
			// both have CritiquePassed=false; entries that passed
			// critique have CritiquePassed=true. When critique stays
			// off, the check is skipped entirely.
			//
			// L.4 Step 2.5: also require CritiqueVersion to be at
			// least the current contract version. Step-2 entries
			// (CritiqueVersion=0) failed under the punt-only gate
			// without ever seeing the hallucination check, so they
			// must be re-analyzed when 2.5 is in effect. The check
			// is no-op when critique is off (consumers who never
			// enabled critique are unaffected).
			critiqueOK := !in.Opts.CritiqueEnabled ||
				(cached.CritiquePassed && cached.CritiqueVersion >= currentCritiqueVersion)
			if cached.ToolCalls >= in.Opts.MinToolCalls && cached.GCSBytes >= in.Opts.MinGCSBytes && critiqueOK {
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
				analysis.CritiquePassed = cached.CritiquePassed
				analysis.CritiqueVersion = cached.CritiqueVersion
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
	// L.4 Step 2.5: when critique is enabled, pre-init the read-tracking
	// maps so findUnreadArtifactCitations runs the check even when the
	// model has made zero successful reads (otherwise its nil-disables
	// contract would silently skip the worst-case hallucination scenario).
	// Pre-init unconditionally would waste an allocation per run for the
	// common critique-disabled path; conditionally is cheap.
	if in.Opts.CritiqueEnabled {
		state.readArtifactsFull = map[string]bool{}
		state.readArtifactsBase = map[string]bool{}
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
	// Per-floor anti-thrash: track the calls + gcsBytes counters at the
	// time we last nudged so we can detect whether the model has made
	// progress on the unmet axis since then. A model that keeps coming
	// back tools-free without progressing on the floor we're complaining
	// about gets its answer accepted (but not cached) so the loop doesn't
	// burn iterations on a refusing model. Sentinel -1 ensures the very
	// first iteration's zero-state always counts as progress.
	nudgedAtCalls := -1
	nudgedAtGCSBytes := -1

	// L.4 Step 2: critique state. state.critiquePassed records whether
	// the accepted final answer cleared the critique gate, used by the
	// cache-write decision below and stamped onto the published
	// AIAnalysis so the build-level shouldReanalyze gate can invalidate
	// uncritiqued entries when critique is enabled later. Stored on
	// state (not local) so stampAgenticTelemetry picks it up via the
	// same path as ToolCalls / GCSBytes / BudgetExhausted. Starts false;
	// set to true only when critique actually runs and passes (so when
	// critique is disabled, it stays false and the cache write logic
	// correctly ignores it via the CritiqueEnabled gate).
	// critiqueRetriesUsed bounds the number of re-prompt rounds per
	// analysis. Each retry extends maxIters by critiqueRetryIters so the
	// model has room to do follow-up tool calls (responding to critique
	// feedback by reading more artifacts is the desired behavior) plus
	// re-emit. Bumping by 1 was too tight: the rubber-duck pass caught
	// that a model reacting to feedback with even one tool call would
	// fall off the end of the loop and end up in runFinalizeRound.
	critiqueRetriesUsed := 0
	maxIters := in.Opts.MaxIters

	for iter := 0; iter < maxIters && !done; iter++ {
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

			// Enforce per-project floors by nudging the model to
			// investigate further before accepting its final answer.
			// Skip the nudge when: (a) no floor is unmet, (b) budgets
			// are already exhausted (a nudge would fight the tool-side
			// "budget exhausted; finalize now" signal), or (c) the
			// model has not made progress on any unmet floor since the
			// last nudge (avoid no-progress loops). The per-axis
			// progress check (rather than calls-only) covers the
			// pathological list-only loop a bytes-floor enables:
			// a model that calls list_artifacts repeatedly raises
			// calls but never gcsBytes, and used to get re-nudged
			// every iteration despite making no real progress.
			floors := evalFloors(state, in.Opts)
			if floors.anyUnmet() && !state.budgetExhausted {
				progressed := false
				if floors.callsUnmet && state.calls > nudgedAtCalls {
					progressed = true
				}
				if floors.gcsUnmet && state.gcsBytes > nudgedAtGCSBytes {
					progressed = true
				}
				if progressed {
					echo := agChatMessage{Role: "assistant"}
					if msg.Content != nil {
						echo.Content = msg.Content
					}
					messages = append(messages, echo, agChatMessage{
						Role:    "user",
						Content: strPtr(formatFloorsNudge(state, in.Opts)),
					})
					nudgedAtCalls = state.calls
					nudgedAtGCSBytes = state.gcsBytes
					log.Printf("  ↻ agentic nudge: tool_calls=%d/min=%d, gcs_kb=%d/min=%d, asking model to investigate further",
						state.calls, in.Opts.MinToolCalls, state.gcsBytes/1024, in.Opts.MinGCSBytes/1024)
					continue
				}
			}

			// L.4 Step 2: critique gate. Catches punt-shaped finals
			// (suggested_fix that's a diagnostic TODO list instead of
			// a concrete remediation) that the prompt rules alone
			// don't catch in weaker models. Mirrors the floor-nudge
			// pattern: re-prompt with feedback, give the model one
			// more iteration to do the work it should have done, and
			// bound by CritiqueMaxRetries to avoid runaway. Only
			// applies when critique is enabled AND the candidate
			// parses cleanly; unparseable finals fall through to the
			// existing finalize-round path below and bypass critique
			// in v1 (rare; can be revisited if shadow A/B data shows
			// post-finalize punts are common).
			if in.Opts.CritiqueEnabled {
				if parsed, ok := tryParseAnalysis(candidate); ok {
					out := critiqueDraft(parsed, state.readArtifactsFull, state.readArtifactsBase)
					if out.Passed {
						state.critiquePassed = true
					} else if critiqueRetriesUsed < in.Opts.CritiqueMaxRetries {
						echo := agChatMessage{Role: "assistant"}
						if msg.Content != nil {
							echo.Content = msg.Content
						}
						messages = append(messages, echo, agChatMessage{
							Role:    "user",
							Content: strPtr(out.Feedback),
						})
						critiqueRetriesUsed++
						maxIters += critiqueRetryIters
						log.Printf("  ✗ agentic critique: %v; re-prompting (retry %d/%d)",
							out.Matches(), critiqueRetriesUsed, in.Opts.CritiqueMaxRetries)
						continue
					} else {
						log.Printf("  ⚠ agentic critique: still failing after %d retries %v; accepting but not caching",
							in.Opts.CritiqueMaxRetries, out.Matches())
					}
				}
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

	// L.4 Step 2 fix (rubber-duck #1): also critique post-loop parsed
	// answers when the in-loop path didn't already mark critique as
	// passed. The in-loop critique only fires on tools-free responses
	// that parse on the spot; outputs from runFinalizeRound (loop maxed
	// out without tools-free) and slow-parse outputs bypass it. Without
	// this check, a punt-shaped finalize-round result publishes-but-
	// never-caches → re-analyzed on every fetcher run → unbounded cost.
	// With it, a passing finalize-round caches normally; a failing one
	// continues the L.3 anti-thrash "publish, don't cache, retry next
	// run" pattern. No retry here: retries are an in-loop concept, and
	// re-entering the loop post-finalize would require a larger refactor
	// that v1 defers.
	if in.Opts.CritiqueEnabled && !state.critiquePassed {
		if out := critiqueDraft(parsed, state.readArtifactsFull, state.readArtifactsBase); out.Passed {
			state.critiquePassed = true
		} else {
			log.Printf("  ⚠ agentic critique: post-loop draft still failing %v; accepting but not caching",
				out.Matches())
		}
	}

	c.cacheAcceptedAnalysis(cacheKey, parsed, state, in.Opts, state.critiquePassed)
	summary, analysis := c.buildOutputs(parsed)
	stampAgenticTelemetry(analysis, state, in.Mode, false, start)
	return summary, analysis, nil
}

// cacheAcceptedAnalysis writes a parsed analysis to the cache, but only if the
// agent met every per-project quality gate. v1 floors: MinToolCalls AND
// MinGCSBytes. v2 (L.4 Step 2) adds the critique gate: when CritiqueEnabled,
// the draft must have passed critique (critiquePassed=true). Below-floor or
// critique-failing finals are still published to the dashboard for this run
// (so triage always has something to show) but are NOT cached, so the next
// fetcher run re-attempts the analysis rather than serving the under-
// investigated or punt-shaped answer.
//
// critiquePassed is meaningful only when opts.CritiqueEnabled; when critique
// is off, its value is ignored.
func (c *Client) cacheAcceptedAnalysis(cacheKey string, parsed analysisResponse, state *agentState, opts AgenticOptions, critiquePassed bool) {
	if evalFloors(state, opts).anyUnmet() {
		return
	}
	if opts.CritiqueEnabled && !critiquePassed {
		return
	}
	version := 0
	if opts.CritiqueEnabled && critiquePassed {
		version = currentCritiqueVersion
	}
	_ = c.cache.Set(cacheKey, agenticCacheData{
		analysisResponse: parsed,
		ToolCalls:        state.calls,
		ModelBytes:       state.modelBytes,
		GCSBytes:         state.gcsBytes,
		BudgetExhausted:  state.budgetExhausted,
		CritiquePassed:   critiquePassed,
		CritiqueVersion:  version,
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

	// L.4 Step 2.5: record successful artifact reads so critiqueDraft
	// can flag prose citations of files the agent never opened. Only
	// counts read_artifact / tail_artifact / grep_artifact (the
	// content-fetching tools); list/find/discover tools are exempt
	// because they don't justify claims about file contents. The
	// payload "error" key check (rubber-duck #1) ensures a failed
	// read can't silently satisfy the hallucination gate.
	if isContentFetchingTool(tc.Function.Name) {
		if _, hasErr := result.Payload["error"]; !hasErr {
			if p := extractToolPathArg(tc.Function.Arguments); p != "" {
				s.recordSuccessfulRead(p)
			}
		}
	}

	return toolEnvelopeJSON(s, result.Payload)
}

// isContentFetchingTool reports whether a tool name corresponds to one
// of the three filesystem read primitives that actually return file
// bytes. Listing tools (list_artifacts, find_artifacts) are excluded:
// seeing a filename in a directory listing does not justify claims
// about the file's contents.
func isContentFetchingTool(name string) bool {
	switch name {
	case "read_artifact", "tail_artifact", "grep_artifact":
		return true
	}
	return false
}

// extractToolPathArg pulls the "path" field out of a content-fetching
// tool's args. Returns "" on parse error or missing field; callers
// treat empty as "don't record". All three content-fetching tools use
// the same `{"path": "..."}` arg shape (see tools/filesystem.go).
func extractToolPathArg(raw string) string {
	if raw == "" {
		return ""
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return ""
	}
	return strings.TrimSpace(args.Path)
}

// recordSuccessfulRead normalizes a successfully-read path and adds it
// to both the full-path and basename indices. When critique is disabled
// the maps remain nil so this function silently no-ops (zero allocation
// on the common path); when critique is enabled doAnalyzeAgentic
// pre-allocates the maps so the hallucination check runs even on runs
// with zero successful reads. See critique.go for normalizeArtifactCitation;
// using the same normalizer here keeps the writer (this function) and
// reader (findUnreadArtifactCitations) trivially consistent.
func (s *agentState) recordSuccessfulRead(rawPath string) {
	if s.readArtifactsFull == nil && s.readArtifactsBase == nil {
		return
	}
	norm := normalizeArtifactCitation(rawPath)
	if norm == "" {
		return
	}
	s.readArtifactsFull[norm] = true
	s.readArtifactsBase[path.Base(norm)] = true
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
