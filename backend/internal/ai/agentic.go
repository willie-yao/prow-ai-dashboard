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
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// AgenticMode is the value stored in models.AIAnalysis.Mode for results
// produced by the agentic pipeline.
const AgenticMode = "agentic"

// UniversalMode is the value stored in models.AIAnalysis.Mode for results
// produced by the use_universal_path flow. Distinct from AgenticMode so
// that flipping a project between the two invalidates previously cached
// analyses (shouldReanalyze treats any mode mismatch as cache-miss).
const UniversalMode = "agentic-universal"

// ErrToolsUnsupported is returned from the agentic loop when the configured
// provider rejects function-calling on the first call (typically HTTP 400
// with a body mentioning "tools" or "functions"). Callers should fall back
// to the single-shot curator pipeline for that failure and avoid retrying
// agentic mode against the same endpoint for the rest of the run.
var ErrToolsUnsupported = errors.New("ai endpoint does not support function calling")

// AgenticOptions is the resolved per-failure budget config. Build via
// project.Agentic.EffectiveAgentic() once per fetcher run and reuse.
type AgenticOptions struct {
	MaxIters        int
	ModelByteBudget int
	GCSByteBudget   int
	Timeout         time.Duration

	// ContextByteBudget caps the estimated serialized request size (system
	// prompt + task + accumulated tool results + reasoning + tool schemas).
	// When the conversation approaches it, the oldest tool-result bodies are
	// elided to a stub so a small-context model does not overflow its window
	// mid-loop. 0 disables compaction (the default; large-context models need
	// no help). Set it to roughly the model's context window in bytes
	// (~3.5-4 bytes/token).
	ContextByteBudget int

	// MinToolCalls is the minimum number of tool calls before a tools-free
	// final answer is accepted as cacheable. Defaults to 0 (no floor). The
	// loop nudges the model with a "you haven't investigated enough" user
	// message and skips the cache write for any final that lands below
	// the floor so the next run gets a fresh attempt.
	MinToolCalls int

	// MinGCSBytes is the minimum cumulative GCS bytes fetched via tool
	// calls before a tools-free final answer is accepted. Complements
	// MinToolCalls because tool-call count alone is gameable: weak models
	// can satisfy a calls floor with cheap list calls or tiny reads. The
	// floor invalidates calls-only finalization. Defaults to 0.
	MinGCSBytes int

	// CritiqueEnabled opts the run into the regex critique gate. After
	// the agentic loop produces a parseable tools-free final, critiqueDraft
	// inspects it; punt-shaped, hallucinated, or import-fabricating
	// answers get re-prompted with targeted feedback up to
	// CritiqueMaxRetries times. Drafts that still fail after retries are
	// published but not cached so the next run gets a fresh attempt.
	CritiqueEnabled bool

	// CritiqueMaxRetries caps the extra re-prompt rounds the loop spends
	// on critique. 0 means "critique once but never retry" (pure don't-
	// cache gate); 2 gets up to 3 total evaluations. Each retry consumes
	// one extra agentic iteration. Only meaningful when CritiqueEnabled.
	CritiqueMaxRetries int

	// SingleToolCall caps the loop to one tool call per assistant turn:
	// extra tool calls in a multi-call model response are dropped (only the
	// first is executed and echoed into history). Needed for endpoints whose
	// chat template rejects multiple tool calls per assistant message (e.g.
	// the stock Llama 3.x Instruct template). Defaults to false so providers
	// that support parallel tool calls keep their efficiency.
	SingleToolCall bool

	// EvidenceInjection makes a critique retry fetch the artifacts the draft
	// cited but never read and embed their content in the retry feedback,
	// instead of only asking the model to read them. Only meaningful when
	// CritiqueEnabled. Adds the fetched bytes to the conversation, so it
	// suits large-context models.
	EvidenceInjection bool
}

// artifactTreeMaxPaths caps how many artifact paths the seed lists, bounding
// the prompt size on builds with very large artifact trees.
const artifactTreeMaxPaths = 500

// artifactTreeNoiseExt are file extensions the seed drops before capping:
// non-text artifacts the model cannot usefully read, so excluding them leaves
// more of the path budget for diagnostic logs.
var artifactTreeNoiseExt = map[string]bool{
	".png": true, ".svg": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".gz": true, ".tar": true, ".tgz": true, ".zip": true, ".bz2": true,
}

// limitToolCalls returns the tool calls the loop should execute and echo this
// turn. With single=true and more than one call, only the first is kept so the
// echoed assistant message stays single-tool-call (required by chat templates
// that reject multiple tool calls per message); the dropped count is returned
// for logging. The model can re-request the dropped calls on a later turn.
func limitToolCalls(calls []agToolCall, single bool) (kept []agToolCall, dropped int) {
	if single && len(calls) > 1 {
		return calls[:1], len(calls) - 1
	}
	return calls, 0
}

// evidenceInjectionMaxArtifacts caps how many artifacts are fetched per
// critique-failure round, bounding the context the injection adds.
const evidenceInjectionMaxArtifacts = 4

// evidenceTreeMaxPaths bounds the single recursive listing used to resolve
// cited basenames and skill-required patterns to real artifact paths, capping
// the GCS list cost of one injection.
const evidenceTreeMaxPaths = 1000

// evidenceInjectionPerArtifactBytes caps the bytes injected per artifact.
const evidenceInjectionPerArtifactBytes = 8 * 1024

// agenticToolBudget caps bytes returned to the model by any single tool
// call. Keeps one runaway response from eating the whole ModelByteBudget.
// 32 KB matches the spike, well above any reasonable JSON envelope.
const agenticToolBudget = 32 * 1024

// critiqueRetryIters is the per-retry budget granted when a critique
// re-prompt is appended. Generous enough for 1-2 follow-up tool calls
// plus the new tools-free final plus slack. Tighter values starve the
// retry where the model elects to investigate before re-emitting.
const critiqueRetryIters = 3

// critiqueMissingEvidenceBonusCap caps the extra iters granted on top of
// critiqueRetryIters for a single missing-evidence retry. Sized to absorb
// realistic recipes with 3-4 evidence groups (1 iter to read each + 1 to
// re-emit) without giving 10-group recipes unbounded budget.
const critiqueMissingEvidenceBonusCap = 6

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

	// ParallelToolCalls is the OpenAI knob requesting at most one tool call
	// per turn when set to false. Sent only when single_tool_call is on.
	// Omitted otherwise so providers keep their default (parallel allowed).
	// Endpoints that honor it let the model pick its single best call;
	// endpoints that ignore it (e.g. some trtllm builds) still return
	// several, which the loop's client-side cap then trims.
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
}

type agChatResponse struct {
	Choices []struct {
		FinishReason string        `json:"finish_reason"`
		Message      agChatMessage `json:"message"`
	} `json:"choices"`
}

func strPtr(s string) *string { return &s }

// ---------- Tool documentation appended to the system prompt ----------

// agToolDocs is the tool-usage strategy section appended to the system
// prompt by the agentic loop. Tool names + descriptions reach the model
// via the schema array; this section adds investigation strategy: drill
// into specifics, don't punt to the user, stop only when evidence is
// genuinely exhausted (not at the first plausible symptom).
const agToolDocs = `

## Tool usage strategy

You have a set of tools for browsing the build's GCS artifact tree (see the
tools field of this request for names, descriptions, and parameters).

1. Start by listing the build root to see what's there.
2. Triage for a known transient FIRST. If the failure matches a transient class named in the project-specific knowledge above (infrastructure flake such as API throttling, quota exhaustion, transient DNS, image-pull backoff, API server / etcd still forming, node not yet registered, or a cleanup-phase deadline), set is_transient=true and stop. Do NOT drill for a code root cause or manufacture a remediation for infrastructure flake; doing so produces a false "real bug" verdict. Only continue to the deep investigation below when the failure is not a known transient. Some symptoms (x509 / "certificate signed by unknown authority", webhook or join timeouts, "connection refused", "context deadline exceeded") occur in BOTH transient flakes and real bugs; the error string alone does not decide. For these, classify by EVIDENCE, not by the string: an error that recovered (later calls succeeded, or the resource eventually reached its desired state), or that the project knowledge names as a known flake, is transient; an error with a specific upstream cause in the related logs (a concrete bootstrap, config, cert, or code failure) is a real bug. When unsure, drill the related logs (the resource's own logs and the owning controller's log; the project-specific section names them) to find that specific cause before deciding; absence of a specific cause favors transient.
3. For multi-MB build-logs, ALWAYS use grep_artifact (with wide surrounding context, e.g. ctx=20), never read_artifact or tail_artifact.
4. Drill into the most relevant named resources. If your current best causal lead depends on a specific resource (a failing Machine, Pod, Node, VM, container, controller, or owning workload), read that resource's own artifacts before finalizing. Do not chase every resource name mentioned in passing; pick the 1-3 most directly tied to the failure. Examples: a failing resource X → read its manifest/status conditions, events, owner-controller log filtered for "X", and any resource-specific runtime logs. The project-specific section names the exact artifact paths these live at. Stopping at the first plausible symptom is the most common failure mode of this tool; treat each symptom as a lead, not the answer.
5. Investigation is YOUR job, not the user's. suggested_fix must be a concrete remediation action (a code change, config edit, command to run, retry, redeploy, rollback, operational fix). It must NOT be a diagnostic or information-gathering task. If the sentence's primary purpose is to learn more (check, verify, investigate, ensure, inspect, examine, confirm, audit, review, look into, determine), it belongs in your tool work BEFORE finalizing, not in suggested_fix. A "then validate by ..." clause is fine, but only after a concrete remediation. If after following the directly relevant artifact leads you still cannot identify a concrete remediation, say so explicitly in suggested_fix and include all three of: (a) the strongest fact you established, (b) the specific artifacts/logs you consulted, (c) the exact missing evidence that prevents a remediation. Do not invoke this escape hatch if any standard remediation or best-evidence operational action is supported by the artifacts you read.
6. Cite actual paths and quoted log lines in your final answer. Do not speculate; if evidence is incomplete, state what is known and what remains unclear.
7. Watch the remaining_model_bytes and remaining_gcs_bytes returned with each tool result; stop browsing and produce the final JSON answer before they hit zero.

Before finalizing, self-check:
- Before drilling, did I rule out a known-transient class named in the project knowledge?
- For an overloaded symptom (cert/x509, webhook or join timeout, connection refused, context deadline), did I check whether it recovered or has a specific upstream cause before deciding is_transient?
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
continuing to investigate.

Output ONLY the JSON object: no prose, no explanation, no markdown fences.
Your entire response must start with { and end with }.`

// formatFloorsNudge builds the user message appended after a tools-free
// model response when one or both per-project floors are unmet. Mentions
// only the axes that are actually unmet so a project configuring only
// MinToolCalls doesn't see a misleading "0 KB" complaint.
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
3. When build-log.txt shows an error, cross-reference the corresponding timestamp in the relevant component or controller log (the project-specific section names where these live). Symptoms surfaced in build-log are often downstream of root causes in the controller.
4. Don't accept the first plausible explanation. Common terminal symptoms (for example kubelet/API-server timeouts, context deadline exceeded, NotReady nodes) usually have earlier upstream causes such as webhook/cert problems, leader-election loss, image pull failures, or missing dependencies. Search nearby logs before concluding.
5. Cite specific file paths and log line numbers in your root_cause. Include enough evidence to explain the causal chain, not just the surface error.

If after this investigation the evidence is genuinely inconclusive, say so explicitly in root_cause rather than speculating.`, strings.Join(unmet, " and "))
}

// agenticCacheData is the on-disk shape of a cached agentic analysis. Embeds
// the raw model response and tags it with per-analysis telemetry so cache
// reads can re-stamp the published AIAnalysis and re-validate against the
// project's current floors.
type agenticCacheData struct {
	analysisResponse
	ToolCalls       int  `json:"tool_calls,omitempty"`
	ModelBytes      int  `json:"model_bytes,omitempty"`
	GCSBytes        int  `json:"gcs_bytes,omitempty"`
	BudgetExhausted bool `json:"budget_exhausted,omitempty"`

	// CritiquePassed marks entries that cleared the critique gate.
	// Defaults to false on pre-critique entries and on entries written
	// while critique was disabled. The cache-read gate uses this to
	// invalidate uncritiqued entries when a consumer later enables
	// critique.
	CritiquePassed bool `json:"critique_passed,omitempty"`

	// CritiqueVersion records which contract version the draft passed
	// critique under. The cache-read gate requires the cached version
	// to be at least currentCritiqueVersion when critique is enabled,
	// so strengthening the gate invalidates entries that passed under
	// the weaker contract.
	CritiqueVersion int `json:"critique_version,omitempty"`

	// SkillSetHash is the fingerprint of the consumer's loaded skill
	// set at the time this draft was accepted. Empty when skills were
	// disabled or no recipes were loaded. Used independently of
	// CritiqueVersion to invalidate cached entries when the consumer
	// edits recipes (engine-side contract unchanged, but the effective
	// evidence requirements drifted).
	SkillSetHash string `json:"skill_set_hash,omitempty"`
}

// floorStatus tracks which per-project floors are currently unmet for a
// given agent state. Used by both the loop's nudge decision and the
// nudge message composer so the two stay in sync.
type floorStatus struct {
	callsUnmet bool
	gcsUnmet   bool
}

func (fs floorStatus) anyUnmet() bool { return fs.callsUnmet || fs.gcsUnmet }

// evalFloors returns which per-project floors the current agent state
// fails to meet. A floor configured as 0 is never reported as unmet.
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

	// critiquePassed records whether the accepted answer cleared the
	// critique gate. Stamped onto the published AIAnalysis so the
	// build-level shouldReanalyze gate can invalidate uncritiqued
	// entries when critique is enabled later. Meaningful only when
	// opts.CritiqueEnabled.
	critiquePassed bool

	// readArtifactsFull / readArtifactsBase track artifacts the agent
	// successfully fetched via read_artifact / tail_artifact /
	// grep_artifact. Used by the critique gate to flag prose citations
	// of files the agent never opened. "full" keeps the directory
	// prefix (catches cross-machine basename collisions); "base" is
	// just path.Base (matches bare-basename citations). Populated only
	// after a successful tool dispatch. Both maps stay nil when
	// critique is disabled to keep the common path zero-allocation.
	readArtifactsFull map[string]bool
	readArtifactsBase map[string]bool

	// skillSet is the loaded recipe set (project-scoped). nil when
	// skills are disabled or no recipes are configured. Held on state
	// so in-loop and post-loop critique paths both consult the same
	// set, and so cacheAcceptedAnalysis / stampAgenticTelemetry can
	// stamp the hash without re-threading it.
	skillSet *skills.Set

	// artifactTreeSetCache is the normalized set of every artifact path
	// in the build, fetched lazily the first time a skill-evidence miss
	// needs to know whether the required evidence even exists in this
	// build. Used to drop recipe requirements that are unsatisfiable
	// because the evidence is absent (recipe inapplicable), so an
	// otherwise-grounded draft is not blocked from caching forever.
	// nil after artifactTreeChecked means "no usable tree" (fetch error
	// or truncated listing), in which case the absence check is skipped.
	artifactTreeSetCache map[string]bool
	artifactTreeChecked  bool
}

// skillAbsenceTreeCap bounds the recursive listing used to decide whether a
// skill's required evidence exists anywhere in the build. Set well above a
// typical build's artifact count so the listing is not truncated; a truncated
// listing disables the absence check (can't prove a path is absent).
const skillAbsenceTreeCap = 5000

// artifactTreeSet returns the normalized set of every artifact path in the
// build, fetched once and cached. Returns nil when the tree cannot be listed
// or was truncated (so callers conservatively skip the absence optimization).
func (s *agentState) artifactTreeSet(ctx context.Context) map[string]bool {
	if s.artifactTreeChecked {
		return s.artifactTreeSetCache
	}
	s.artifactTreeChecked = true
	if s.browser == nil {
		return nil
	}
	paths, truncated, err := s.browser.ListTree(ctx, skillAbsenceTreeCap)
	if err != nil || truncated {
		return nil
	}
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		if norm := normalizeArtifactCitation(p); norm != "" {
			set[norm] = true
		}
	}
	s.artifactTreeSetCache = set
	return set
}

func (s *agentState) modelRemaining() int { return s.opts.ModelByteBudget - s.modelBytes }
func (s *agentState) gcsRemaining() int   { return s.opts.GCSByteBudget - s.gcsBytes }

// stampAgenticTelemetry copies per-call counters onto the AIAnalysis so the
// published JSON exposes per-failure cost. Called at every successful exit
// point (cache hit, normal finish, finalize-round finish, synthesized
// fallback). An empty mode defaults to AgenticMode; UniversalMode is passed
// explicitly by the universal path.
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
		if state.skillSet != nil {
			analysis.SkillSetHash = state.skillSet.Hash()
		}
	}
}

// ---------- Public entry point ----------

// AgenticInputs bundles the per-failure context required by the agentic loop.
// Lifetime notes:
//   - Browser, Cache, and WebURLBase are scoped to one build.
//   - Registry and EnabledTools are scoped to one Service (built once per
//     project at fetcher startup).
//   - Opts and Skills are per-project.
//   - Mode is the value stamped on the returned AIAnalysis (defaults to
//     AgenticMode; UniversalMode is passed by the universal path so cache
//     invalidation kicks in when consumers flip the switch).
type AgenticInputs struct {
	Browser      artifacts.Browser
	Opts         AgenticOptions
	Registry     *tools.Registry
	EnabledTools []string
	Cache        *tools.Cache
	WebURLBase   string
	Mode         string

	// Skills is the consumer's loaded recipe set. nil disables skill
	// matching entirely (also the case when critique is disabled, since
	// recipes are only consulted inside the critique gate). Skills.Hash()
	// is stamped onto cached entries so consumer-side recipe edits
	// invalidate cache without an engine version bump.
	Skills *skills.Set
}

// ---------- Context-window compaction ----------

const (
	// compactionTargetRatio is the fraction of ContextByteBudget compaction
	// drives toward once triggered, leaving headroom so it does not re-fire
	// every iteration.
	compactionTargetRatio = 0.7
	// compactionKeepRecentTools tool results are kept at full content when
	// possible so the model always has its latest evidence verbatim.
	compactionKeepRecentTools = 3
	// compactionStubHead is how many leading bytes of an elided tool result
	// are retained as a hint (usually the envelope head with the artifact
	// path/status) before the elision note.
	compactionStubHead = 160
	// compactionMsgOverhead approximates per-message JSON framing bytes.
	compactionMsgOverhead = 48
)

// elisionMarker tags a stubbed message so compaction is idempotent across
// iterations and tests can detect elision.
const elisionMarker = "bytes elided to fit context"

func isStubbed(c *string) bool {
	return c != nil && strings.Contains(*c, elisionMarker)
}

// stubContent keeps a short head of the original tool result plus an elision
// note that tells the model how to recover the evidence.
func stubContent(orig string) string {
	head := orig
	if len(head) > compactionStubHead {
		head = head[:compactionStubHead]
	}
	return fmt.Sprintf("%s\n...[%d %s; re-call the tool if you need this evidence again]",
		head, len(orig)-len(head), elisionMarker)
}

// schemaPayloadBytes is the serialized size of the tool schemas sent on every
// loop call. Computed once per loop and added to the size estimate so
// compaction accounts for the fixed schema cost, not just message content.
func schemaPayloadBytes(schemas []tools.Schema) int {
	if len(schemas) == 0 {
		return 0
	}
	b, err := json.Marshal(schemas)
	if err != nil {
		return 0
	}
	return len(b)
}

// requestSizeEstimate approximates the serialized chat-request size in bytes:
// message content + tool-call arguments + per-message framing + the fixed
// schema payload.
func requestSizeEstimate(messages []agChatMessage, schemaBytes int) int {
	total := schemaBytes + 64 // request framing
	for i := range messages {
		total += compactionMsgOverhead
		if messages[i].Content != nil {
			total += len(*messages[i].Content)
		}
		for _, tc := range messages[i].ToolCalls {
			total += len(tc.Function.Name) + len(tc.Function.Arguments) + 32
		}
	}
	return total
}

// compactMessages elides accumulated tool-result (and, if still over budget,
// assistant-reasoning) content so the estimated request stays under
// budgetBytes, preventing context-window overflow on small-context models.
// Disabled when budgetBytes <= 0. Preserves the system prompt (index 0) and
// the task (index 1), and never reorders messages or rewrites tool_call_id
// wiring, so the OpenAI tool-call pairing stays valid. Returns the slice and
// the number of messages elided this call.
func compactMessages(messages []agChatMessage, schemaBytes, budgetBytes int) ([]agChatMessage, int) {
	if budgetBytes <= 0 || requestSizeEstimate(messages, schemaBytes) <= budgetBytes {
		return messages, 0
	}
	target := int(float64(budgetBytes) * compactionTargetRatio)
	elided := 0

	// Tool-result messages, oldest first, that are not already stubbed.
	var toolIdx []int
	for i := 2; i < len(messages); i++ {
		if messages[i].Role == "tool" && messages[i].Content != nil && !isStubbed(messages[i].Content) {
			toolIdx = append(toolIdx, i)
		}
	}
	stub := func(i int) {
		messages[i].Content = strPtr(stubContent(*messages[i].Content))
		elided++
	}
	// Stage 1: stub older tool results, preferring to keep the most recent
	// compactionKeepRecentTools verbatim.
	keepFrom := len(toolIdx) - compactionKeepRecentTools
	for p := 0; p < keepFrom && requestSizeEstimate(messages, schemaBytes) > target; p++ {
		stub(toolIdx[p])
	}
	// Stage 2: still over target, so stub the recent tool results too.
	for p := 0; p < len(toolIdx) && requestSizeEstimate(messages, schemaBytes) > target; p++ {
		if !isStubbed(messages[toolIdx[p]].Content) {
			stub(toolIdx[p])
		}
	}
	// Stage 3: still over target, so stub older assistant reasoning, keeping
	// the tool_calls wiring intact.
	for i := 2; i < len(messages) && requestSizeEstimate(messages, schemaBytes) > target; i++ {
		m := &messages[i]
		if m.Role == "assistant" && len(m.ToolCalls) > 0 && m.Content != nil &&
			!isStubbed(m.Content) && len(*m.Content) > compactionStubHead {
			stub(i)
		}
	}
	return messages, elided
}

// doAnalyzeAgentic runs the tool-calling AI loop for one failure. Returns
// the same (summary, analysis) pair as doAnalyze so callers can treat both
// pipelines uniformly.
//
// The caller is responsible for constructing a fresh Browser per failure
// (typically via artifacts.Factory.ForBuild) and for choosing the cache key
// (which MUST encode build+failure so two builds of the same test never
// share an agentic cache entry).
//
// Returns ErrToolsUnsupported wrapped on the first API call if the endpoint
// rejects function-calling. The caller should fall back to doAnalyze for
// that failure.
func (c *Client) doAnalyzeAgentic(
	ctx context.Context,
	in AgenticInputs,
	cacheKey, sysPrompt, userPrompt string,
) (*models.AISummary, *models.AIAnalysis, error) {
	start := time.Now()
	if raw, ok := c.cache.Get(cacheKey); ok {
		var cached agenticCacheData
		if json.Unmarshal(raw, &cached) == nil {
			// Re-validate the cache hit against the current floors and
			// critique contract. Raising any floor, enabling critique,
			// bumping currentCritiqueVersion, or editing the recipe set
			// all invalidate previously cached entries on read.
			critiqueOK := !in.Opts.CritiqueEnabled ||
				(cached.CritiquePassed && cached.CritiqueVersion >= currentCritiqueVersion)
			// Skills feed only the critique gate, so the recipe-set hash
			// is part of the cache contract exactly when critique is on:
			// editing recipes then invalidates prior entries on read.
			if in.Opts.CritiqueEnabled {
				wantHash := ""
				if in.Skills != nil {
					wantHash = in.Skills.Hash()
				}
				if cached.SkillSetHash != wantHash {
					critiqueOK = false
				}
			}
			if cached.ToolCalls >= in.Opts.MinToolCalls && cached.GCSBytes >= in.Opts.MinGCSBytes && critiqueOK {
				summary, analysis := c.buildOutputs(cached.analysisResponse)
				stampAgenticTelemetry(analysis, nil, in.Mode, true, start)
				// Restore the recorded per-analysis telemetry so the
				// published JSON keeps its tool-call/cost/budget-exhausted
				// signals across cache hits; without this, hits would
				// publish ToolCalls=0 and shouldReanalyze would re-trigger
				// reanalysis on every run.
				analysis.ToolCalls = cached.ToolCalls
				analysis.ModelBytes = cached.ModelBytes
				analysis.GCSBytes = cached.GCSBytes
				analysis.BudgetExhausted = cached.BudgetExhausted
				analysis.CritiquePassed = cached.CritiquePassed
				analysis.CritiqueVersion = cached.CritiqueVersion
				analysis.SkillSetHash = cached.SkillSetHash
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
	// Skills are consulted only inside the critique gate, so load the
	// recipe set into the run exactly when critique is enabled (recipe
	// presence is the opt-in; an empty set is a no-op).
	if in.Opts.CritiqueEnabled {
		state.skillSet = in.Skills
	}
	// Pre-init the read-tracking maps when critique is enabled so
	// findUnreadArtifactCitations runs the check even when the model has
	// made zero successful reads (otherwise its nil-disables contract
	// would silently skip the worst-case hallucination scenario).
	if in.Opts.CritiqueEnabled {
		state.readArtifactsFull = map[string]bool{}
		state.readArtifactsBase = map[string]bool{}
	}

	fullSysPrompt := sysPrompt + agToolDocs
	// Always seed the build's artifact path list into the prompt so the
	// model reads exact paths instead of guessing leaf filenames (the
	// dominant cause of failed deep reads). Deterministic, capped, and a
	// no-op when the listing is empty or fails.
	if seed := c.buildArtifactTreeSeed(ctx, in.Browser); seed != "" {
		userPrompt = seed + "\n\n---\n\n" + userPrompt
	}
	messages := []agChatMessage{
		{Role: "system", Content: strPtr(fullSysPrompt)},
		{Role: "user", Content: strPtr(userPrompt)},
	}
	schemas := state.registry.Schemas(state.enabledTools)

	loopCtx, cancel := context.WithDeadline(ctx, state.startTime.Add(in.Opts.Timeout))
	defer cancel()

	var finalContent string
	var done bool
	// Per-floor anti-thrash: track the calls + gcsBytes counters at the
	// time we last nudged so we can detect whether the model has made
	// progress on the unmet axis since then. A model that keeps coming
	// back tools-free without progressing gets accepted (but not cached)
	// so the loop doesn't burn iterations on a refusing model. Sentinel
	// -1 ensures the very first iteration's zero-state counts as progress.
	nudgedAtCalls := -1
	nudgedAtGCSBytes := -1

	// critiqueRetriesUsed bounds the re-prompt rounds per analysis. Each
	// retry extends maxIters by critiqueRetryIters (plus a bonus when
	// the retry is satisfying missing skill evidence) so the model has
	// room to do follow-up tool calls plus re-emit.
	critiqueRetriesUsed := 0
	maxIters := in.Opts.MaxIters

	// Fixed schema cost added to every size estimate so compaction budgets
	// against the real request, not just message content.
	schemaBytes := schemaPayloadBytes(schemas)

	// When single_tool_call is on, request parallel_tool_calls=false so
	// compliant endpoints emit a single call (the model picks its best one);
	// the client-side cap below still trims endpoints that ignore the flag.
	var parallelToolCalls *bool
	if in.Opts.SingleToolCall {
		f := false
		parallelToolCalls = &f
	}

	for iter := 0; iter < maxIters && !done; iter++ {
		if in.Opts.ContextByteBudget > 0 {
			var elided int
			messages, elided = compactMessages(messages, schemaBytes, in.Opts.ContextByteBudget)
			if elided > 0 {
				log.Printf("  ✂ context compaction: elided %d message(s) to fit ~%d-byte window", elided, in.Opts.ContextByteBudget)
			}
		}
		resp, err := c.callChatWithTools(loopCtx, messages, schemas, parallelToolCalls)
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
			// are exhausted (would fight the tool-side "finalize now"
			// signal), or (c) the model has not progressed on any unmet
			// floor since the last nudge. The per-axis progress check
			// covers the pathological list-only loop: a model calling
			// list_artifacts repeatedly raises calls but never gcsBytes
			// and would otherwise be re-nudged every iteration.
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

			// Critique gate. Re-prompts the model with targeted feedback
			// when the draft punts, hallucinates, fabricates an import
			// path, or fails recipe-driven evidence. Only fires on
			// parseable candidates; unparseable finals fall through to
			// runFinalizeRound below.
			if in.Opts.CritiqueEnabled {
				if parsed, ok := tryParseAnalysis(candidate); ok {
					matchedSkills := matchSkillsForDraft(state, parsed)
					out := critiqueDraft(parsed, state.readArtifactsFull, state.readArtifactsBase, matchedSkills)
					if len(out.MissingSkillEvidence) > 0 {
						if treeSet := state.artifactTreeSet(loopCtx); treeSet != nil {
							if n := pruneAbsentSkillEvidence(parsed, &out, treeSet); n > 0 {
								log.Printf("  ⓘ skill-evidence: %d required group(s) absent from this build's artifacts; not held against the draft", n)
							}
						}
					}
					if out.Passed {
						state.critiquePassed = true
					} else if critiqueRetriesUsed < in.Opts.CritiqueMaxRetries {
						echo := agChatMessage{Role: "assistant"}
						if msg.Content != nil {
							echo.Content = msg.Content
						}
						// Evidence injection: fetch the evidence the draft
						// cited but never read (and skill-required evidence it
						// skipped) and append it to the feedback, so a model
						// that ignores "go read X" still gets the bytes.
						// Best-effort; failures fall back to the plain text
						// feedback.
						feedback := out.Feedback
						if in.Opts.EvidenceInjection {
							if inj := c.buildEvidenceInjection(loopCtx, state, out); inj != "" {
								feedback = feedback + "\n\n" + inj
							}
						}
						messages = append(messages, echo, agChatMessage{
							Role:    "user",
							Content: strPtr(feedback),
						})
						critiqueRetriesUsed++
						// Extend the retry budget proportional to the
						// number of missing evidence groups. Plain
						// re-prompts stay at critiqueRetryIters; skill-
						// driven re-prompts get a bonus capped at
						// critiqueMissingEvidenceBonusCap so 10-group
						// recipes don't unbound the loop.
						extra := critiqueRetryIters
						if missing := out.MissingEvidenceCount(); missing > 0 {
							bonus := 1 + 2*missing
							if bonus > critiqueMissingEvidenceBonusCap {
								bonus = critiqueMissingEvidenceBonusCap
							}
							extra += bonus
						}
						maxIters += extra
						log.Printf("  ✗ agentic critique: %v; re-prompting (retry %d/%d, +%d iters)",
							out.Matches(), critiqueRetriesUsed, in.Opts.CritiqueMaxRetries, extra)
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

		toolCalls, dropped := limitToolCalls(msg.ToolCalls, in.Opts.SingleToolCall)
		if dropped > 0 {
			log.Printf("  ⤵ single_tool_call: model returned %d tool calls; executing the first and dropping %d (model may re-request them)",
				len(msg.ToolCalls), dropped)
		}

		echo := agChatMessage{Role: "assistant", ToolCalls: toolCalls}
		if msg.Content != nil {
			echo.Content = msg.Content
		}
		messages = append(messages, echo)

		for _, tc := range toolCalls {
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
		finalContent = c.runFinalizeRound(loopCtx, messages, in.Opts.ContextByteBudget)
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

	// Also critique post-loop parsed answers when the in-loop path didn't
	// already mark critique as passed. The in-loop critique only fires on
	// tools-free responses that parse on the spot; outputs from
	// runFinalizeRound and slow-parse outputs would otherwise bypass it
	// and publish-but-never-cache forever.
	if in.Opts.CritiqueEnabled && !state.critiquePassed {
		matchedSkills := matchSkillsForDraft(state, parsed)
		out := critiqueDraft(parsed, state.readArtifactsFull, state.readArtifactsBase, matchedSkills)
		if len(out.MissingSkillEvidence) > 0 {
			if treeSet := state.artifactTreeSet(loopCtx); treeSet != nil {
				if n := pruneAbsentSkillEvidence(parsed, &out, treeSet); n > 0 {
					log.Printf("  ⓘ skill-evidence: %d required group(s) absent from this build's artifacts; not held against the draft", n)
				}
			}
		}
		switch {
		case out.Passed:
			state.critiquePassed = true
		case in.Opts.EvidenceInjection && (len(out.UnreadCitations) > 0 || len(out.MissingSkillEvidence) > 0):
			// The force-finalized draft cited artifacts the agent never read,
			// or skipped evidence its claimed failure class requires. Fetch
			// that evidence, inject it, and run one more finalize round so the
			// model can re-ground its answer. This is the dominant post-loop
			// failure on weak models.
			if inj := c.buildEvidenceInjection(loopCtx, state, out); inj != "" {
				messages = append(messages,
					agChatMessage{Role: "assistant", Content: strPtr(finalContent)},
					agChatMessage{Role: "user", Content: strPtr(out.Feedback + "\n\n" + inj)})
				revised := c.runFinalizeRound(loopCtx, messages, in.Opts.ContextByteBudget)
				rp, ok2 := tryParseAnalysis(revised)
				if !ok2 {
					// The model sometimes answers the injected evidence in
					// prose instead of JSON on the first finalize. One more
					// attempt with the same JSON-only prompt usually lands it.
					revised = c.runFinalizeRound(loopCtx, messages, in.Opts.ContextByteBudget)
					rp, ok2 = tryParseAnalysis(revised)
				}
				if ok2 {
					parsed = rp
					out2 := critiqueDraft(parsed, state.readArtifactsFull, state.readArtifactsBase, matchSkillsForDraft(state, parsed))
					if len(out2.MissingSkillEvidence) > 0 {
						if treeSet := state.artifactTreeSet(loopCtx); treeSet != nil {
							pruneAbsentSkillEvidence(parsed, &out2, treeSet)
						}
					}
					if out2.Passed {
						state.critiquePassed = true
					} else {
						log.Printf("  ⚠ agentic critique: post-injection draft still failing %v; accepting but not caching", out2.Matches())
					}
				} else {
					log.Printf("  ⚠ agentic critique: post-injection finalize did not parse after retry; keeping prior draft, not caching")
				}
			} else {
				log.Printf("  ⚠ agentic critique: post-loop draft still failing %v; no fetchable evidence to inject; accepting but not caching", out.Matches())
			}
		default:
			log.Printf("  ⚠ agentic critique: post-loop draft still failing %v; accepting but not caching",
				out.Matches())
		}
	}

	c.cacheAcceptedAnalysis(cacheKey, parsed, state, in.Opts, state.critiquePassed)
	summary, analysis := c.buildOutputs(parsed)
	stampAgenticTelemetry(analysis, state, in.Mode, false, start)
	return summary, analysis, nil
}

// buildArtifactTreeSeed returns a prompt addendum listing the build's
// artifact path tree (capped), so the model reads exact paths instead of
// guessing leaf filenames. Over-fetches, drops non-text noise, then caps to
// artifactTreeMaxPaths so the budget holds diagnostic logs. Tells the model
// to read from the list directly rather than spend tool calls on
// list_artifacts / find_artifacts rediscovering paths it already has. One
// recursive listing; degrades to "" if the listing is empty or fails (the
// loop proceeds with its normal prompt).
func (c *Client) buildArtifactTreeSeed(ctx context.Context, browser artifacts.Browser) string {
	if browser == nil {
		return ""
	}
	// Over-fetch so the noise filter below can reclaim budget for real logs.
	raw, rawTruncated, err := browser.ListTree(ctx, artifactTreeMaxPaths*2)
	if err != nil || len(raw) == 0 {
		if err != nil {
			log.Printf("  ⓘ artifact-tree seed skipped: %v", err)
		}
		return ""
	}
	paths := make([]string, 0, len(raw))
	for _, p := range raw {
		if artifactTreeNoiseExt[strings.ToLower(path.Ext(p))] {
			continue
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return ""
	}
	sort.Strings(paths)
	truncated := rawTruncated
	if len(paths) > artifactTreeMaxPaths {
		paths = paths[:artifactTreeMaxPaths]
		truncated = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Artifact paths for this build (%d file(s)). These are the EXACT paths to pass to read_artifact / tail_artifact / grep_artifact; do NOT guess paths, and do NOT spend tool calls on list_artifacts / find_artifacts to rediscover paths that are already listed here. Read the relevant logs directly:\n", len(paths))
	for _, p := range paths {
		b.WriteString(p)
		b.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&b, "... [list truncated at %d paths; use list_artifacts only for subtrees not shown above]\n", artifactTreeMaxPaths)
	}
	log.Printf("  🗂 artifact-tree seed: %d path(s) injected", len(paths))
	return b.String()
}

// buildEvidenceInjection fetches the evidence a critique-failing draft needed
// but did not read, and returns a feedback addendum embedding it. It covers
// two buckets: (1) artifacts the draft cited but never read, and (2) evidence
// a matched skill requires for the claimed failure class. Full-path citations
// are fetched directly; bare-basename citations and skill-required patterns
// are resolved to real paths with a single bounded tree walk (so cost does
// not scale with the number of targets). Fetched paths are marked read so the
// next critique pass does not re-flag them. Returns "" when nothing could be
// fetched. Intentionally not gated on the model byte budget: critique runs
// after the investigation has spent it, and this evidence is the highest-value
// content at that moment; the per-artifact and count caps bound the addition.
func (c *Client) buildEvidenceInjection(ctx context.Context, state *agentState, out critiqueOutcome) string {
	var sections []string
	fetched := 0
	tail := func(p string) string {
		if fetched >= evidenceInjectionMaxArtifacts {
			return ""
		}
		res, err := state.browser.Tail(ctx, p, 200, evidenceInjectionPerArtifactBytes)
		if err != nil || res == nil || len(res.Content) == 0 {
			return ""
		}
		return string(res.Content)
	}
	add := func(realPath, label, content string) {
		state.gcsBytes += len(content)
		state.modelBytes += len(content)
		state.recordSuccessfulRead(realPath)
		sections = append(sections, fmt.Sprintf("### %s\n%s", label, content))
		fetched++
	}

	// walkTarget pairs a path predicate with the label to use when it matches.
	type walkTarget struct {
		match func(string) bool
		label func(real string) string
	}
	var targets []walkTarget

	// Bucket 1: cited-but-unread artifacts. Fetch full paths directly; queue
	// bare basenames (and full paths that fail to fetch) for the walk.
	for _, cited := range out.UnreadCitations {
		if strings.Contains(cited, "/") {
			if content := tail(cited); content != "" {
				add(cited, cited+" (tail)", content)
				continue
			}
		}
		base := path.Base(cited)
		citedCopy := cited
		targets = append(targets, walkTarget{
			match: func(p string) bool { return strings.EqualFold(path.Base(p), base) },
			label: func(real string) string {
				return fmt.Sprintf("%s (tail; nearest match for cited %q)", real, citedCopy)
			},
		})
	}

	// Bucket 2: skill-required evidence groups the agent did not satisfy.
	for _, m := range out.MissingSkillEvidence {
		skillID := m.Skill.ID
		for _, g := range m.Missing {
			group := g
			targets = append(targets, walkTarget{
				match: func(p string) bool { return group.Satisfied(map[string]bool{strings.ToLower(p): true}) },
				label: func(real string) string {
					return fmt.Sprintf("%s (tail; required evidence %q for skill %q)", real, group.ID, skillID)
				},
			})
		}
	}

	// Single bounded walk resolves every remaining target's first match.
	if len(targets) > 0 && fetched < evidenceInjectionMaxArtifacts {
		preds := make([]func(string) bool, len(targets))
		for i := range targets {
			preds[i] = targets[i].match
		}
		matches := resolveEvidenceByWalk(ctx, state.browser, preds)
		for i, real := range matches {
			if real == "" || fetched >= evidenceInjectionMaxArtifacts {
				continue
			}
			if content := tail(real); content != "" {
				add(real, targets[i].label(real), content)
			}
		}
	}

	if fetched == 0 {
		return ""
	}
	log.Printf("  📎 evidence injection: fetched %d artifact(s) into the retry", fetched)
	return "The engine fetched evidence you cited but had not read, and/or evidence required for this failure class. Ground your root_cause in what these artifacts ACTUALLY show below; correct or drop any claim they do not support.\n\n" + strings.Join(sections, "\n\n")
}

// resolveEvidenceByWalk lists the build's artifact tree once (recursive, no
// delimiter) and returns, for each predicate, the first matching real path (or
// "" if unmatched). Bounded by evidenceTreeMaxPaths to cap GCS list cost. Stops
// early once every predicate has a match.
func resolveEvidenceByWalk(ctx context.Context, browser artifacts.Browser, preds []func(string) bool) []string {
	found := make([]string, len(preds))
	remaining := len(preds)
	paths, _, err := browser.ListTree(ctx, evidenceTreeMaxPaths)
	if err != nil {
		return found
	}
	for _, p := range paths {
		if remaining == 0 {
			break
		}
		for i, pred := range preds {
			if found[i] == "" && pred(p) {
				found[i] = p
				remaining--
			}
		}
	}
	return found
}

// matchSkillsForDraft joins the candidate draft's prose fields and matches
// them against the loaded recipe set. Returns nil if skills are disabled or
// no recipes are loaded. Used by both the in-loop and post-loop critique so
// both paths match against the same draft text.
func matchSkillsForDraft(state *agentState, parsed analysisResponse) []skills.Skill {
	if state == nil || state.skillSet == nil {
		return nil
	}
	return state.skillSet.Match(strings.Join(parsed.proseFields(), "\n"))
}

// cacheAcceptedAnalysis writes a parsed analysis to the cache, but only if
// the agent met every per-project quality gate (floors + critique). Below-
// floor or critique-failing finals are still published to the dashboard for
// this run (so triage always has something to show) but are NOT cached, so
// the next run re-attempts them. critiquePassed is ignored when
// opts.CritiqueEnabled is false.
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
	skillHash := ""
	if state.skillSet != nil {
		skillHash = state.skillSet.Hash()
	}
	_ = c.cache.Set(cacheKey, agenticCacheData{
		analysisResponse: parsed,
		ToolCalls:        state.calls,
		ModelBytes:       state.modelBytes,
		GCSBytes:         state.gcsBytes,
		BudgetExhausted:  state.budgetExhausted,
		CritiquePassed:   critiquePassed,
		CritiqueVersion:  version,
		SkillSetHash:     skillHash,
	})
}

// runFinalizeRound asks the model for one more no-tools response containing
// just the final JSON. Used when the agent ran out of iterations or returned
// prose without parseable JSON. Returns the raw content (which may itself be
// unparseable; callers handle that).
func (c *Client) runFinalizeRound(ctx context.Context, messages []agChatMessage, contextByteBudget int) string {
	messages = append(messages, agChatMessage{Role: "user", Content: strPtr(agForceFinalizePrompt)})
	if contextByteBudget > 0 {
		// The finalize round sends no tool schemas, so estimate against
		// messages alone.
		messages, _ = compactMessages(messages, 0, contextByteBudget)
	}
	resp, err := c.callChatWithTools(ctx, messages, nil, nil)
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
// path. Sleeps the same callDelay between calls to be a good citizen. When
// parallelToolCalls is non-nil it is sent as the OpenAI parallel_tool_calls
// flag (used to request single tool calls on compliant endpoints).
func (c *Client) callChatWithTools(ctx context.Context, messages []agChatMessage, toolDefs []tools.Schema, parallelToolCalls *bool) (*agChatResponse, error) {
	time.Sleep(callDelay)

	body, err := json.Marshal(agChatRequest{Model: c.model, Messages: messages, Tools: toolDefs, ParallelToolCalls: parallelToolCalls})
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

	// Record successful artifact reads so critiqueDraft can flag prose
	// citations of files the agent never opened. Only content-fetching
	// tools count; list/find tools don't justify content claims. The
	// "error" key check prevents a failed read from silently satisfying
	// the hallucination gate.
	if isContentFetchingTool(tc.Function.Name) {
		if _, hasErr := result.Payload["error"]; !hasErr {
			if p := extractToolPathArg(tc.Function.Arguments); p != "" {
				s.recordSuccessfulRead(p)
			}
		}
	}

	// Optional per-tool-call trace for diagnosing investigation behavior.
	// Off unless AGENTIC_TRACE_TOOLS is set, so production logs stay clean.
	if os.Getenv("AGENTIC_TRACE_TOOLS") != "" {
		flag := "ok"
		if _, hasErr := result.Payload["error"]; hasErr {
			flag = "ERROR"
		}
		log.Printf("    🔧 %s(%s) -> %d gcs bytes [%s]", tc.Function.Name, truncate(tc.Function.Arguments, 140), result.BytesFetched, flag)
	}

	return toolEnvelopeJSON(s, result.Payload)
}

// isContentFetchingTool reports whether a tool name is one of the three
// filesystem read primitives that actually return file bytes. Listing
// tools are excluded: a directory listing doesn't justify content claims.
func isContentFetchingTool(name string) bool {
	switch name {
	case "read_artifact", "tail_artifact", "grep_artifact":
		return true
	}
	return false
}

// extractToolPathArg pulls the "path" field out of a content-fetching tool's
// args. Returns "" on parse error or missing field. All three content-
// fetching tools use the same `{"path": "..."}` arg shape.
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

// recordSuccessfulRead normalizes a successfully-read path and adds it to
// both the full-path and basename indices. Silent no-op when critique is
// disabled (maps are nil). Uses the same normalizeArtifactCitation as
// findUnreadArtifactCitations so writer and reader stay consistent.
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
