# Agentic AI mode (tool calling)

Agentic mode is an alternative to the curator-driven evidence pipeline.
Instead of the engine pre-fetching a fixed set of artifacts based on
`ai.evidence` in `project.yaml`, the model decides what to read by
calling four function-calling tools that browse the build's GCS artifact
tree directly: `list_artifacts`, `read_artifact`, `tail_artifact`, and
`grep_artifact`.

It is opt-in and off by default. The curator path remains the default
because it is cheaper (~1/10 the tokens, ~1/7 the wall clock) and
adequate for failures with well-structured per-cluster artifacts.
Agentic mode is meant for the long-tail failures where the curator's
hardcoded list misses what actually matters.

## When to enable it

Enable agentic mode if any of the following are true for your project:

- Your prow jobs publish artifacts in a layout that doesn't fit the
  `clusters/<name>/machines/<vm>/...` shape the `capi` curator expects,
  and you don't want to write a new AI module.
- A meaningful share of your failures happen before any cluster is
  created (pre-flight checks, image build, controller startup), so
  `ClusterArtifacts` is empty on those test cases.
- You've been iterating on `ai.evidence` and the model still says "I'd
  need more logs" in summaries.

If your curator config is producing useful summaries today, leave
agentic mode off.

## Endpoint requirements

Agentic mode requires the AI endpoint to implement OpenAI-style
function calling (`tools` field on the request, `tool_calls` field on
the response). Verified endpoints:

- **GitHub Copilot** (`api.githubcopilot.com`) — supported.
- **OpenAI** — supported on all models since gpt-3.5-turbo-0613.
- **Azure OpenAI** — supported on tool-calling-capable deployments.
- **Ollama / vLLM / NIMs** — supported per-model; check your model card.

The engine probes lazily: the first agentic call to an endpoint that
returns HTTP 400/422 with a tools-related error message is treated as
a soft capability miss. The fetcher logs
`AI endpoint rejected tools; falling back to curator for this run` and
runs every subsequent failure that run through the curator instead. No
restart needed; flip your config back to `enabled: false` on the next
deploy.

## Configuring it

All knobs live under `ai.agentic` in `project.yaml`. Every field is
optional except `enabled`:

```yaml
ai:
  module: "capi"
  agentic:
    enabled: true                 # turn agentic on
    always: false                 # if true, run agentic on every failure
    max_iters: 15                 # tool-call rounds per failure
    model_byte_budget: 300000     # total bytes of tool output sent to the model
    gcs_byte_budget: 1000000000   # total bytes fetched from GCS
    wall_clock: 5m                # per-failure agentic wall-clock cap
    min_tool_calls: 0             # minimum tool calls before a final answer is accepted
    min_gcs_bytes: 0              # minimum GCS bytes fetched before a final answer is accepted
    critique:
      enabled: false              # opt into the L.4 Step 2 punt-detection gate
      max_retries: 2              # re-prompt rounds before accepting a still-punting draft
```

Defaults match the spike that validated the design and are conservative
enough that you almost never need to tune them. Lower `max_iters` first
if you see the model loop without converging; raise `gcs_byte_budget` if
your builds have very large logs and grep is being cut short.

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

### `always: true` vs `always: false`

- `always: true` routes every failure through agentic regardless of the
  module's preference. Use for end-to-end validation against a small
  dataset, or for projects where curator coverage is poor across the
  board. Expect ~10x the AI bill of curator mode.
- `always: false` lets the AI module decide per-failure via its optional
  `AgenticPreferrer` implementation. Modules without one (currently the
  `generic` module) never go agentic in this mode, so `always: false`
  with `module: generic` is effectively a no-op.

The `capi` module's `PrefersAgentic` returns true when:

1. `ClusterArtifacts` is nil on the test case (collector never matched
   a cluster, usually meaning a pre-cluster failure), or
2. `ClusterArtifacts` is present but both `Machines` and `PodLogDirs`
   are empty (cluster name known but no per-machine or per-controller
   logs collected).

Both cases reduce the curator prompt to little more than the bare
build-log; the agentic loop can hunt through the artifact tree on
demand and reliably do better.

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
(`agentic:<module>:<job>:<build>:<hash>` vs
`comprehensive:<hash>` / `analyze:<module>:<hash>`). Switching a
project between modes does not re-analyze instantly; the engine
detects the cached `mode` mismatch on the next fetcher run and
re-analyzes the failure under the new mode.

Cached agentic entries are scoped to a specific build because answers
cite build-specific paths and line numbers; the same test failing in
two different builds gets two separate agentic analyses. Cached
curator entries are not build-scoped (the prompt content is largely
deterministic given the test + failure message).

## Troubleshooting

- **No agentic entries are appearing.** Confirm `agentic.enabled: true`
  and either `agentic.always: true` or a module that implements
  `AgenticPreferrer`. Check the fetcher logs for either
  `Agentic AI enabled (...)` at startup or per-failure
  `opted into agentic mode: ...` lines.
- **Every failure logs "AI endpoint rejected tools".** The endpoint
  doesn't support function calling. Either switch endpoints or set
  `agentic.enabled: false` to silence the log line.
- **Costs spiked.** Drop `agentic.always: true` to `false`, or lower
  `max_iters`. Inspect the cached analyses for `mode: "agentic"` to
  estimate how much of the bill is agentic vs curator.
- **Model loops without finalizing.** Lower `max_iters` and check
  whether the forced-finalize round produces a useful answer. If not,
  the `prompts/system.md` may not give the model enough structure to
  conclude; tighten its triage instructions.

## Implementation reference

- `backend/internal/ai/agentic.go` — the tool-calling loop, finalize
  round, and JSON repair.
- `backend/internal/artifacts/` — the `Browser` interface and
  `GCSBrowser` implementation backing the four tools.
- `backend/internal/ai/modules/capi/agentic.go` — the `capi` module's
  `PrefersAgentic` heuristic.
- `backend/cmd/ai-toolcall-spike/` — the throwaway prototype the
  production design was validated against. Useful for spot-checking
  agentic answers against a single failure without redeploying.
