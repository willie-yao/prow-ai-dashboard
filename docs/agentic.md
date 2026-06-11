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
    timeout: 5m                   # per-failure agentic wall-clock timeout
    min_tool_calls: 0             # minimum tool calls before a final answer is accepted
    min_gcs_bytes: 0              # minimum GCS bytes fetched before a final answer is accepted
    single_tool_call: false       # send at most one tool call per turn (for single-tool-call-only models)
    critique:
      enabled: false              # opt into the deterministic critique gate
      max_retries: 2              # re-prompt rounds before accepting a still-failing draft
    skills:
      enabled: false              # opt into the recipe-driven evidence gate
                                  # (loads <project_dir>/skills/*.yaml; see docs/skills.md)
    evidence_injection: false     # on a critique retry, fetch+inject cited-but-unread artifacts
```

Defaults match the spike that validated the design and are conservative
enough that you almost never need to tune them. Lower `max_iters` first
if you see the model loop without converging. The byte budgets (model
output, compaction, and the GCS fetch ceiling) are not configurable: the
first two auto-size from the endpoint's context window and the GCS ceiling
is a fixed engine safety cap.

### Automatic budget sizing

The agentic loop bounds how much tool output the model accumulates (the
evidence cap) and compacts old tool results before the request would
overflow the model's context window (the compaction guard). **Neither is
configurable** — the engine sizes them automatically: at startup it GETs
the endpoint's `/v1/models`, reads the served model's `context_window`
(tokens), converts it to bytes (~4 bytes/token), and sets the evidence cap
to ~50% and the compaction guard to ~75% of the window. The same config
therefore works against a 40K, 128K, or 256K deployment with no tuning. If
the endpoint doesn't expose `/v1/models` or omits `context_window` (e.g.
GitHub Copilot), the engine falls back to a static evidence cap (300000
bytes) with compaction off.

The budgets are client-side on purpose: an OpenAI-compatible server
(Dynamo / vLLM / TRT-LLM) enforces its window as a *hard* limit and 500s on
overflow rather than degrading, so the loop must compact *before* reaching
it. Auto-sizing just removes the per-deployment hand-tuning.

The compaction guard works by estimating each request's serialized size
(system prompt + task + accumulated tool results + reasoning + tool
schemas) and, before it would exceed the budget, eliding the oldest
tool-result bodies to a short stub (head + a "re-call the tool if you need
this" note). This keeps a long, critique-heavy investigation from
overflowing the window mid-loop and failing with an empty analysis.


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

The byte counter is the same `gcs_bytes` counter the engine uses for
its internal GCS fetch ceiling, so the floor is measured against bytes
actually pulled from GCS by `read_artifact`, `tail_artifact`, and
`grep_artifact`. `list_artifacts` contributes 0.

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

#### L.4 Step 2.5 strengthening: hallucinated citations

L.4 Step 2 dropped Qwen3-235B's punt rate from 80% to 0% on CAPZ
but exposed a new failure mode (Case 1 of the Step 2 A/B): a
draft that passes the punt regex with high confidence but cites
an artifact it never read (`actuators.go` it never opened),
emitting a wrong-but-fluent root cause. Step 2.5 adds a
deterministic check that runs alongside the punt regex and
combines into one retry message:

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

> A second Step 2.5 check (a regex that flagged Go-import-style
> prefixes such as `sigs.k8s.io/` or `github.com/` in
> `relevant_files`/prose as "fabricated import paths") was later
> removed: with the artifact-tree seed and a stronger model it
> caught zero real fabrications and instead misfired on legitimate
> upstream URLs the model cited for provenance (e.g. the clusterctl
> `core-components.yaml` release asset), which needlessly failed
> grounded analyses and collapsed the agentic cache rate.

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

**Inapplicable recipes do not block caching.** A recipe whose
required evidence does not exist anywhere in the build's artifact
tree is inapplicable to that build: the agent cannot read evidence
the run never produced. When a matched recipe has a missing
evidence group, the engine does one bounded recursive listing of
the build tree and drops any group whose `any_of` patterns match no
path in it. Only groups whose evidence **exists but was not read**
remain a genuine miss. Without this, a recipe that triggers on the
root cause but requires (say) a cluster YAML the failed run never
dumped would fail critique on every run, so the analysis was
published but never cached and re-analyzed forever. The listing is
cached per analysis and only fetched when a skill miss actually
occurs; a truncated listing disables the check (the engine cannot
prove a path is absent), preserving the stricter behavior.

See [`docs/skills.md`](skills.md) for the full schema, authoring
guidance, and observability notes.

### `single_tool_call`

Off by default. When enabled, the loop sends at most one tool call per
assistant turn. Two mechanisms work together: the request sets the OpenAI
`parallel_tool_calls: false` flag (so endpoints that honor it let the model
pick its single best call at generation time), and as a fallback for
endpoints that ignore the flag, the loop executes and echoes only the first
tool call when several come back at once (the rest are dropped and can be
re-requested on a later turn). Set this for endpoints whose chat template
rejects multiple tool calls in one assistant message. The stock Llama 3.x
Instruct template, for example, raises `This model only supports single
tool-calls at once!` and the provider surfaces it as a 500 once a
multi-tool-call assistant turn is replayed in history. This is a property of
the model's own chat template (the Llama tool-calling format is one call per
turn), not a provider bug, so the fix belongs in the loop. (Observed: some
trtllm/Dynamo builds accept `parallel_tool_calls: false` but ignore it, which
is exactly why the client-side cap is also needed.) Leave it off for
providers that support parallel tool calls (Copilot, OpenAI, Claude) so they
keep their round-trip efficiency.

### `evidence_injection`

Off by default; requires `critique.enabled`. The critique gate already
detects when a draft cites an artifact (a `.log`/`.txt`/`.json`/`.xml` path)
that the agent never actually read, and re-prompts the model to go read it.
Weak models frequently ignore that instruction and re-emit the same
ungrounded claim. When `evidence_injection` is on, the engine instead
**fetches** each cited-but-unread artifact (the model already named the
path), caps it, and embeds its content directly in the retry feedback:
"here is what it actually shows; ground your root_cause in it or drop the
claim." The fetched paths are marked read, so the next critique pass does
not re-flag them.

This converts an ignored "go read X" loop into "here is X", which is the
single most common reason drafts fail critique on weaker models (citing
evidence they never opened). It covers two buckets: artifacts the draft
**cited but never read**, and evidence a **matched skill requires** for the
claimed failure class. Full-path citations are fetched directly; bare-
basename citations and skill-required patterns are resolved to real paths
with a single bounded tree walk (so cost does not scale with the number of
targets). It runs on both the in-loop critique retry and the post-loop
force-finalize path (where weak models most often land after exhausting
their tool-call budget), in the latter case driving one extra finalize round
with the injected evidence. If that post-injection finalize comes back as
prose instead of JSON, the engine retries it once (the force-finalize prompt
demands a JSON-only response: no prose, no markdown fences) before giving up,
so a one-off formatting slip does not discard an otherwise-cacheable answer.
It adds the fetched bytes (up to a few capped
artifacts per retry) to the conversation, so it is best suited to
large-context models. Best-effort: a path that cannot be resolved or fetched
is skipped and the plain text feedback still applies. No cache-version
interaction; it only changes the retry prompt.

### Artifact-tree seeding (always on)

The engine always fetches the build's full artifact path list (one recursive
GCS listing) and prepends it to the analysis prompt, so the model starts with
the **exact** paths to pass to `read_artifact` / `tail_artifact` /
`grep_artifact` instead of guessing leaf filenames. On weaker models,
guessed-and-wrong paths are a leading cause of failed deep reads: the model
navigates to the right directory but invents a filename that does not exist, so
it never reaches the controller/machine log holding the upstream cause. Seeding
the real tree removes the guessing. It is not configurable.

The listing is capped (currently 500 paths) to bound prompt size; a build
with more artifacts is truncated with a note pointing the model at
`list_artifacts` for the rest. Before capping, the engine over-fetches and
drops non-text noise (images and archives such as `.png`, `.svg`, `.gz`,
`.tar`, `.zip`) the model cannot usefully read, leaving more of the path
budget for diagnostic logs. The seed header also tells the model to read from
the list directly and **not** spend tool calls on `list_artifacts` /
`find_artifacts` rediscovering paths it already has. It adds the path list (a
few KB to tens of KB) to the prompt. Degrades to a no-op if the listing is
empty or fails (the loop proceeds with its normal prompt). One extra listing
per uncached failure; no cache-version interaction.

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

Hitting a byte-budget cap mid-loop triggers a forced finalize round:
the engine drops the `tools` field and asks the model for its final
JSON answer based on whatever it has seen so far. This always produces
a usable analysis, since incomplete is better than absent. Hitting the
`timeout`, by contrast, cancels the in-flight request and the analysis
errors out for that failure, so set it generously enough that a healthy
investigation finishes (raise it for slow or contended endpoints).

### `ai.concurrency` (parallel analysis)

Failures are analyzed sequentially by default, so a full cold-cache
fetch takes roughly `failures x 30-90s`. Each analysis is an
independent sequence of model round-trips, so `ai.concurrency: N` runs
up to N investigations at once. A batching endpoint (self-hosted vLLM /
TRT-LLM, which serve many requests on one GPU via continuous batching)
absorbs this and wall-clock drops roughly in proportion until the
endpoint saturates; a value of 4-6 is a good starting point for a
dedicated endpoint.

Defaults to **1** (sequential): the engine has no request-level backoff,
so a shared, rate-limited provider (e.g. GitHub Copilot) can return 429
under parallelism. Raise it only for endpoints you control. The setting
is independent of the fetcher's `-workers` flag, which parallelizes the
artifact *fetch* phase, not analysis. It applies to both curator and
agentic analysis. Concurrency does not change results or cache
semantics; the AI cache, per-build tool caches, and the
tools-unsupported flag are all internally synchronized.

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
