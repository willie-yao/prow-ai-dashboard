# Agentic AI mode (tool calling)

Agentic mode lets the LLM decide which artifacts to read instead of
pre-fetching a fixed set. The model calls four function-calling tools
that browse the build's GCS artifact tree: `list_artifacts`,
`read_artifact`, `tail_artifact`, and `grep_artifact`. Optional tier-2
tools add Kubernetes-shaped discovery (`discover_clusters`,
`discover_controllers`, etc.).

Enable it with `ai.use_universal_path: true` + `ai.agentic.enabled: true`
for the typical "let the model figure it out" flow. The non-universal
opt-in flow is available too for projects that want a custom AI module
to decide per-failure whether to go agentic.

## When to enable it

Agentic mode is the recommended starting point for any new consumer.
Set `ai.use_universal_path: true` to get the full agentic flow with no
curator pre-fetch step (the model browses everything itself via the
registered tools). The universal flow is what both production CAPZ
dashboards use today.

## Endpoint requirements

Agentic mode requires the AI endpoint to implement OpenAI-style
function calling (`tools` field on the request, `tool_calls` field on
the response). Verified endpoints:

- **GitHub Copilot** (`api.githubcopilot.com`) — supported.
- **OpenAI** — supported on all models since gpt-3.5-turbo-0613.
- **Azure OpenAI** — supported on tool-calling-capable deployments.
- **Ollama / vLLM / NIMs** — supported per-model; check your model card.

Under `use_universal_path: true` there is no curator fallback: an
endpoint that rejects tools surfaces as an explicit "unavailable"
summary in the dashboard rather than silently degrading. The
non-universal opt-in flow does fall back to curator on a tools-not-
supported error and logs `AI endpoint rejected tools; falling back to
curator for this run`.

## Configuring it

All knobs live under `ai.agentic` in `project.yaml`. Every field is
optional except `enabled`:

```yaml
ai:
  use_universal_path: true        # recommended for new consumers
  agentic:
    enabled: true                 # required even under use_universal_path
    always: false                 # if true, run agentic on every failure
    max_iters: 15                 # tool-call rounds per failure
    model_byte_budget: 300000     # total bytes of tool output sent to the model
    gcs_byte_budget: 1000000000   # total bytes fetched from GCS
    context_byte_budget: 0        # 0 = off; cap total request size to fit a small model window
    wall_clock: 5m                # per-failure agentic wall-clock cap
    min_tool_calls: 0             # minimum tool calls before a final answer is accepted
    min_gcs_bytes: 0              # minimum GCS bytes fetched before a final answer is accepted
    single_tool_call: false       # send at most one tool call per turn (for single-tool-call-only models)
    critique:
      enabled: false              # opt into the deterministic critique gate
      max_retries: 2              # re-prompt rounds before accepting a still-failing draft
    skills:
      enabled: false              # opt into the recipe-driven evidence gate
                                  # (loads <project_dir>/skills/*.yaml; see docs/skills.md)
```

Defaults match the spike that validated the design and are conservative
enough that you almost never need to tune them. Lower `max_iters` first
if you see the model loop without converging; raise `gcs_byte_budget` if
your builds have very large logs and grep is being cut short.

`context_byte_budget` is off by default and only matters for models with
a small context window. When set, the loop estimates each request's
serialized size (system prompt + task + accumulated tool results +
reasoning + tool schemas) and, before it would exceed the budget, elides
the oldest tool-result bodies to a short stub (head + a "re-call the tool
if you need this" note). This keeps a long, critique-heavy investigation
from overflowing the window mid-loop and failing with an empty analysis.
Set it to roughly the model's context window in bytes (~3 bytes/token is
a safe ratio for dense CI logs), staying under the hard token limit so
the trigger fires before an overflow.


### `min_tool_calls`

A minimum-investigation floor. When the model returns a final answer
with fewer than this many tool calls, the loop appends a nudge
("you have only made N tool calls, investigate further before
finalizing") and re-prompts. Below-floor finals are still published
(so triage always shows SOMETHING) but are NOT written to the AI cache,
so the next fetcher run retries the analysis fresh.

Default is `0` — no floor. Strong tool-using models (e.g. Claude Opus)
already investigate deeply on the universal path (~9 tool calls
median); the floor is unnecessary and would add noise. Weaker
open-weights models (e.g. Qwen3-235B) tend to finalize from the
prompt alone in 0-2 tool calls, fabricating wrong root causes. Set
`min_tool_calls: 3` (or higher) for these endpoints to force at least
some artifact inspection.

Anti-thrash: the loop only re-nudges if the model has issued new tool
calls since the last nudge. A model that ignores the nudge and
immediately re-returns its tools-free answer is accepted (and not
cached) rather than looping until `max_iters` is exhausted.

Cache invalidation: bumping `min_tool_calls` on an existing project
invalidates any cached entries with a lower tool-call count on the
next fetcher run; they re-analyze automatically. Invalidation happens
at two layers:

- The agentic AI cache (`data/ai_cache.json`) is re-validated on each
  read; pre-floor entries (which have no `tool_calls` field, default
  to zero) are treated as a miss for any non-zero floor.
- The build-cache test data (`data/jobs/*.json`) already carries the
  prior run's `AIAnalysis` attached to each failure. When the cached
  analysis's `tool_calls` falls below the current floor AND the
  desired mode is agentic, the build-cache entry is also re-analyzed
  rather than being served as-is. Without this layer, pre-floor
  per-test analyses would skip the agentic cache check entirely and
  bypass the floor forever.

### `min_gcs_bytes`

A minimum-evidence-bytes floor. Complements `min_tool_calls` because
tool-call count alone is gameable: a weaker model can satisfy a calls
floor with cheap `list_artifacts` calls or `read_artifact` requests
on a default 8 KB length and still finalize without meaningful
evidence. Observed against Dynamo-hosted Qwen3-235B: 6 tool calls
returning 13 KB total, then a fabricated "no specific error found"
root cause on a failure where Claude (same build) found the actual
webhook x509 cert mismatch from 9 MB of logs.

The byte counter is the same `gcs_bytes` counter the engine already
uses for cost capping (`gcs_byte_budget`), so the floor is measured
against bytes actually pulled from GCS by `read_artifact`,
`tail_artifact`, and `grep_artifact`. `list_artifacts` contributes 0.

Default is `0` — no floor. A reasonable starting value for weaker
models is `200000` (200 KB); raise gradually if the model keeps
parking at the floor with shallow evidence. Don't over-tune: bytes
are a proxy for investigation depth, not a guarantee of evidence
quality (a 500 KB grep with zero useful matches still satisfies the
floor).

Same publish-and-don't-cache semantics as `min_tool_calls`. Same
two-layer cache invalidation (agentic cache + build-cache test data)
when the floor is raised. The two floors are combined with AND: an
analysis must meet BOTH to be cached and to bypass re-analysis on
the next run.

Anti-thrash: progress is tracked per floor. A model that calls
`list_artifacts` in a loop raises `tool_calls` but never `gcs_bytes`,
and used to risk being re-nudged every iteration. The current loop
re-nudges only if the model has made progress on the specific axis
that is still unmet; if neither calls nor bytes have advanced since
the last nudge, the answer is accepted (but not cached).

### `critique`

A punt-detection gate that runs after the model produces a parseable
tools-free final. Catches a residual failure mode in weaker models
where `suggested_fix` is a diagnostic / information-gathering TODO
list ("Check X. Verify Y. Investigate Z.") rather than a concrete
remediation, despite the system prompt explicitly forbidding this
shape. The check is a deterministic regex (see
`backend/internal/ai/critique.go`); no extra LLM call.

When the regex matches, the loop appends targeted feedback that
quotes the offending suggested_fix back to the model, lists the
exact phrases that tripped the gate, and re-states the two
allowed shapes (concrete remediation OR the strict no-remediation
escape hatch). It then re-prompts; each retry consumes one extra
agentic iteration on top of `max_iters`. Drafts that still punt
after `max_retries` retries are published but NOT cached, so the
next fetcher run retries with a fresh attempt.

Defaults to disabled. Recommended for weaker open-weights models
that consistently punt despite the prompt-side rules (Qwen3-235B
post-L.4-Step-1 measured at 80% punt rate on CAPZ failures, vs
40% for Claude Opus on the same cases). Strong tool-using models
benefit too but the cost / behavior trade-off is per-consumer:
when enabled, expect 1.0-1.5x baseline iterations for the typical
failure (most analyses pass critique on the first try; only the
punts incur retries).

`max_retries` defaults to `2` when `enabled: true`. Note that the
field follows the `min_tool_calls` / `min_gcs_bytes` "0 = use
default" convention: writing `max_retries: 0` in YAML is
indistinguishable from omitting the field, so both yield the
engine default. To disable retries entirely (treat critique as a
pure don't-cache gate with no re-prompting), turn the whole
feature off (`critique.enabled: false`) — the gate-only mode is
a future option that the v1 implementation does not surface.

Cache invalidation: enabling `critique` on an existing project
invalidates any cached entries that didn't pass critique (which
includes ALL pre-L.4-Step-2 entries, since they were written with
no critique field; defaults to `false` on read). Same two-layer
behavior as the floor invalidations (agentic AI cache + build-cache
test data). Disabling critique does NOT invalidate previously
critique-passed entries; they serve from cache as usual.

Coverage: critique runs both in-loop (on tools-free finals that
parse on the spot, with re-prompt retries) AND post-loop (on
outputs from `runFinalizeRound` when the agentic loop maxed out
without finalizing). The post-loop check is single-shot — it
gates caching but doesn't re-prompt — so a punt-shaped
finalize-round result publishes, doesn't cache, and re-analyzes
on the next fetcher run (same anti-thrash trade-off as the floor
gates).

#### L.4 Step 2.5 strengthening: hallucinated citations + fabricated import paths

L.4 Step 2 dropped Qwen3-235B's punt rate from 80% to 0% on CAPZ
but exposed a new failure mode (Case 1 of the Step 2 A/B): a
draft that passes the punt regex with high confidence but cites
an artifact it never read (`actuators.go` it never opened),
emitting a wrong-but-fluent root cause. Step 2.5 adds two
deterministic checks that run alongside the punt regex and
combine into one retry message:

1. **Hallucinated artifact citations.** The agentic loop records
   the path of every successful `read_artifact` / `tail_artifact`
   / `grep_artifact` call. Critique then scans the draft's
   `root_cause`, `summary`, `suggested_fix`, and each
   `relevant_files` entry for artifact-shaped tokens (`.log`
   files plus the known Prow artifacts: `build-log.txt`,
   `clone-log.txt`, `started.json`, `finished.json`,
   `prowjob.json`, `junit_*.xml`). Source files (`.go`, `.yaml`,
   generic `.json`) are excluded because they legitimately live
   in the source repo, not the artifact tree. A citation that
   includes a directory prefix must match a full read path
   exactly (catches the cross-machine basename-collision case:
   reading `machine-a/boot.log` then citing `machine-b/boot.log`
   fails). A bare basename matches any read with the same
   basename. Failed reads (tool returned `{"error": ...}`)
   do NOT count as reads, so a model cannot launder a citation
   by reading a non-existent file.
2. **Fabricated Go-import paths in `relevant_files`.** Entries
   prefixed with `sigs.k8s.io/`, `github.com/`, `k8s.io/`,
   `golang.org/`, or `google.golang.org/` are flagged: that field
   is supposed to hold repo-relative source paths, and the L.4
   Step 2 Case 1 hallucination used a GOPATH-shaped prefix on a
   file that didn't exist. The check rejects the format; the
   model is asked to re-emit with the correct repo-relative
   path or omit the entry.

When the agentic loop runs with `critique.enabled: true`, the
read-tracking maps are pre-allocated even before the first
successful read, so the hallucination check is active from the
first tools-free final. When critique is disabled the maps stay
nil and the check is a free no-op.

Cache versioning: a `critique_version` int is stamped onto every
critique-passing analysis (currently `4` = L.4 Step 3). The
build-cache and per-failure-cache invalidation gates both reject
entries whose `critique_version` is below the current engine's
version. This guarantees that strengthening the gate
(e.g. adding a new check in a future L.4 Step) automatically
invalidates entries that passed under the older, weaker
contract, without needing per-consumer cache clears.

#### L.4 Step 3 strengthening: consumer-owned skill / recipe registry

L.4 Step 2.5 cleaned up structural hallucinations but couldn't
catch semantic ones (model reads an artifact and draws the wrong
conclusion, e.g. "API throttling" when the build-log clearly
shows x509 errors). Step 3 adds a consumer-side knowledge layer:
each project ships YAML "skills" (recipes) under
`<project_dir>/skills/*.yaml`. When a recipe's regex triggers
match the model's draft, the critique gate enforces that the
agent has read evidence the recipe declares canonical for the
pattern. Missing evidence appends a per-recipe feedback block
(with procedure quoted under a "consumer guidance, not engine
instruction" disclaimer) and dynamically extends the retry budget
so the agent has room to satisfy the missing evidence in the
next round.

Skills are opt-in via `ai.agentic.skills.enabled: true`. They
extend the critique gate, so they only fire when `critique.enabled`
is also true. Cache invalidation: every cache entry carries a
`skill_set_hash` fingerprint of the loaded recipe set; consumer
edits to any recipe change the hash and invalidate affected entries
on the next run, independently of the engine-side
`critique_version` bump.

See [`docs/skills.md`](skills.md) for the full schema, authoring
guidance, and observability notes.

### `single_tool_call`

Off by default. When enabled, the loop sends at most one tool call per
assistant turn: if the model returns several tool calls in a single
response, only the first is executed and echoed into the conversation
history, and the rest are dropped (the model can re-request them on a later
turn). Set this for endpoints whose chat template rejects multiple tool
calls in one assistant message. The stock Llama 3.x Instruct template, for
example, raises `This model only supports single tool-calls at once!` and
the provider surfaces it as a 500 once a multi-tool-call assistant turn is
replayed in history. This is a property of the model's own chat template
(the Llama tool-calling format is one call per turn), not a provider bug, so
the fix belongs in the loop. Leave it off for providers that support
parallel tool calls (Copilot, OpenAI, Claude) so they keep their round-trip
efficiency.

### `always: true` vs `always: false`

- `always: true` routes every failure through agentic regardless of the
  module's preference. Use for end-to-end validation against a small
  dataset, or for projects where curator coverage is poor across the
  board. Expect ~10x the AI bill of curator mode.
- `always: false` lets the AI module decide per-failure via its optional
  `AgenticPreferrer` implementation. Modules without one (currently the
  `generic` module) never go agentic in this mode, so `always: false`
  with `module: generic` is effectively a no-op. Under
  `use_universal_path: true`, agentic is forced on for every failure
  regardless of this field.

## Cost and behavior

Per failure, agentic mode uses roughly 50-150k input tokens (vs 3-15k
for curator) and runs for 30-90 seconds wall clock (vs 5-15s for
curator). The exact numbers depend on artifact size and how deep the
model digs.

Hitting any budget cap or wall-clock cap mid-loop triggers a forced
finalize round: the engine drops the `tools` field and asks the model
for its final JSON answer based on whatever it has seen so far. This
always produces a usable analysis — incomplete is better than absent.

## Cache semantics

Agentic and curator analyses are cached under different keys
(`agentic:<module>:<job>:<build>:<hash>` vs `analyze:<module>:<hash>`).
Switching a project between modes does not re-analyze instantly; the
engine detects the cached `mode` mismatch on the next fetcher run and
re-analyzes the failure under the new mode.

Cached agentic entries are scoped to a specific build because answers
cite build-specific paths and line numbers; the same test failing in
two different builds gets two separate agentic analyses. Cached
curator entries are not build-scoped (the prompt content is largely
deterministic given the test + failure message).

## Troubleshooting

- **No agentic entries are appearing.** Confirm `agentic.enabled: true`
  and either `use_universal_path: true`, `agentic.always: true`, or a
  module that implements `AgenticPreferrer`. Check the fetcher logs for
  either `Universal AI path enabled (...)` / `Agentic AI enabled (...)`
  at startup.
- **Every failure logs "AI endpoint rejected tools".** The endpoint
  doesn't support function calling. Either switch endpoints or set
  `agentic.enabled: false` to silence the log line. Under
  `use_universal_path: true` this surfaces as an "unavailable" summary
  for every failure instead.
- **Costs spiked.** Drop `agentic.always: true` to `false`, or lower
  `max_iters`. Inspect the cached analyses for `mode: "agentic"` /
  `"agentic-universal"` to estimate how much of the bill is agentic.
- **Model loops without finalizing.** Lower `max_iters` and check
  whether the forced-finalize round produces a useful answer. If not,
  the `prompts/system.md` may not give the model enough structure to
  conclude; tighten its triage instructions.

## Implementation reference

- `backend/internal/ai/agentic.go` — the tool-calling loop, finalize
  round, and JSON repair.
- `backend/internal/ai/critique.go` — the deterministic critique gate.
- `backend/internal/ai/skills/` — the recipe-driven evidence layer.
- `backend/internal/ai/modules/universal/` — the project-agnostic AI
  module used under `use_universal_path: true`.
- `backend/internal/artifacts/` — the `Browser` interface and
  `GCSBrowser` implementation backing the filesystem tools.
- `backend/cmd/ai-toolcall-spike/` — the throwaway prototype the
  production design was validated against. Useful for spot-checking
  agentic answers against a single failure without redeploying.
