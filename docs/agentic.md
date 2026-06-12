# Agentic AI analysis (tool calling)

The agentic loop is the engine's only analysis path: the LLM decides which
artifacts to read instead of pre-fetching a fixed set. The model calls four
function-calling tools that browse the build's GCS artifact tree:
`list_artifacts`, `read_artifact`, `tail_artifact`, and `grep_artifact`.
Optional tier-2 tools add Kubernetes-shaped discovery (`discover_clusters`,
`discover_controllers`, etc.).

There is nothing to enable: if `-ai` is on and the endpoint supports
function calling, every failure is analyzed by the agentic loop. The model
browses everything itself via the registered tools; the per-failure prompt
is just the failing test's context.

## Endpoint requirements

Agentic analysis requires the AI endpoint to implement OpenAI-style
function calling (`tools` field on the request, `tool_calls` field on
the response). Verified endpoints:

- **GitHub Copilot** (`api.githubcopilot.com`) — supported.
- **OpenAI** — supported on all models since gpt-3.5-turbo-0613.
- **Azure OpenAI** — supported on tool-calling-capable deployments.
- **Ollama / vLLM / NIMs** — supported per-model; check your model card.

There is no tools-free fallback: an endpoint that rejects function calling
surfaces as an explicit "AI analysis unavailable" summary in the dashboard
rather than silently degrading.

## Configuration

All knobs are inlined directly under `ai:` in `project.yaml`. Every field is
optional; the agentic loop runs with engine defaults when none are set:

```yaml
ai:
  endpoint: ...                 # optional; env AI_ENDPOINT / default Copilot
  model: ...                    # optional; env AI_MODEL / default Copilot model
  concurrency: 1                # parallel analyses (raise for endpoints you control)
  max_iters: 15                 # tool-call rounds per failure
  timeout: 5m                   # per-failure agentic wall-clock timeout
  min_tool_calls: 0             # minimum tool calls before a final answer is accepted
  min_gcs_bytes: 0              # minimum GCS bytes fetched before a final answer is accepted
  single_tool_call: false       # send at most one tool call per turn (for single-tool-call-only models)
  critique:
    enabled: false              # opt into the deterministic critique gate
                                # (auto-enabled when skills/*.yaml recipes are present)
    max_retries: 2              # re-prompt rounds before accepting a still-failing draft
  evidence_injection: false     # on a critique retry, fetch+inject cited-but-unread artifacts
  tools: [filesystem, k8s]      # registered tool groups exposed to the model
```

The defaults are conservative enough that you almost never need to tune them.
Each field below is the one-line summary; see [How it works](#how-it-works)
for the underlying mechanics. The byte budgets (model output, compaction, and
the GCS fetch ceiling) are **not** configurable: the first two auto-size from
the endpoint's context window and the GCS ceiling is a fixed engine safety cap
(see [Automatic budget sizing](#automatic-budget-sizing)).

### `max_iters`

Tool-call rounds per failure. Default `15`. Lower it first if the model loops
without converging. Critique retries add iterations on top of this.

### `timeout`

Per-failure wall-clock cap. Default `5m`. Hitting it cancels the in-flight
request and errors the analysis out (unlike a budget cap, which forces a
graceful finalize), so set it generously for slow or contended endpoints.

### `min_tool_calls`

Minimum tool calls before a final answer is accepted. Default `0` (no floor).
Below-floor finals are published but not cached, so the next run retries.
Leave at 0 for strong models; set `3` or higher for weaker open-weights models
that finalize from the prompt alone. See [Investigation floors](#investigation-floors).

### `min_gcs_bytes`

Minimum bytes fetched from GCS before a final answer is accepted. Default `0`
(no floor). Complements `min_tool_calls` because call count alone is gameable
(a model can satisfy it with cheap `list_artifacts` calls or tiny reads).
`200000` (200 KB) is a reasonable starting value for weaker models. See
[Investigation floors](#investigation-floors).

### `critique`

Opt into the deterministic critique gate, which rejects punt-shaped and
ungrounded drafts and re-prompts up to `max_retries` times. Defaults to
disabled; `max_retries` defaults to `2` when enabled. **Auto-enabled when
skill recipes are present.** Recommended for weaker open-weights models that
punt despite the prompt rules; strong tool-using models rarely need it. See
[The critique gate](#the-critique-gate).

### `evidence_injection`

On a critique retry, fetch the artifacts the draft cited but never read and
embed their content in the retry feedback ("here is what it actually shows").
Off by default; requires `critique.enabled`. Best suited to large-context
models. See [Evidence injection](#evidence-injection).

### `single_tool_call`

Send at most one tool call per assistant turn. Off by default. Required for
endpoints whose chat template rejects multiple tool calls in one assistant
message (e.g. the stock Llama 3.x Instruct template); leave it off for
providers that support parallel tool calls (Copilot, OpenAI, Claude). See
[Single tool call](#single-tool-call).

### `tools`

Which registered tool groups the model can call. Defaults to
`[filesystem, k8s]`. Narrow to `[filesystem]` for non-Kubernetes projects
whose artifact tree has no cluster resource YAMLs (the k8s tier-2 tools would
return empty).

### `concurrency`

How many failures to analyze in parallel. Defaults to `1` (sequential). Raise
only for endpoints you control; a shared, rate-limited provider can 429 under
parallelism. See [Parallel analysis](#parallel-analysis).

---

## How it works

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

### Investigation floors

`min_tool_calls` and `min_gcs_bytes` are minimum-investigation floors. When
the model returns a final answer below a floor, the loop appends a nudge
("you have only made N tool calls / fetched N KB, investigate further before
finalizing") and re-prompts. Below-floor finals are still published (so
triage always shows SOMETHING) but are NOT written to the AI cache, so the
next fetcher run retries the analysis fresh. The two floors are combined with
AND: an analysis must meet BOTH to be cached and to bypass re-analysis.

Why two floors: tool-call count alone is gameable. A weaker model can satisfy
a calls floor with cheap `list_artifacts` calls or `read_artifact` requests on
a default 8 KB length and still finalize without meaningful evidence (observed:
6 tool calls returning 13 KB total, then a fabricated "no specific error found"
root cause on a failure where a stronger model found the actual webhook x509
cert mismatch from 9 MB of logs). The byte floor is measured against bytes
actually pulled from GCS by `read_artifact`, `tail_artifact`, and
`grep_artifact`; `list_artifacts` contributes 0. Bytes are a proxy for depth,
not a guarantee of quality (a 500 KB grep with zero useful matches still
satisfies the floor), so raise gradually rather than over-tuning.

**Anti-thrash.** Progress is tracked per floor. A model that calls
`list_artifacts` in a loop raises `tool_calls` but never `gcs_bytes`. The loop
re-nudges only if the model has made progress on the specific axis that is
still unmet; if neither calls nor bytes have advanced since the last nudge,
the answer is accepted (but not cached) rather than looping until `max_iters`
is exhausted.

**Cache invalidation (two layers).** Raising a floor on an existing project
invalidates cached entries below it on the next fetcher run:

- The agentic AI cache (`data/ai_cache.json`) is re-validated on each read;
  pre-floor entries (no `tool_calls`/`gcs_bytes` field, default zero) are
  treated as a miss for any non-zero floor.
- The build-cache test data (`data/jobs/*.json`) carries the prior run's
  `AIAnalysis` on each failure. When the cached analysis falls below the
  current floor, the build-cache entry is also re-analyzed rather than served
  as-is. Without this layer, pre-floor per-test analyses would bypass the
  floor forever.

### The critique gate

A punt-detection gate that runs after the model produces a parseable
tools-free final. Catches a residual failure mode in weaker models where
`suggested_fix` is a diagnostic / information-gathering TODO list ("Check X.
Verify Y. Investigate Z.") rather than a concrete remediation, despite the
system prompt forbidding this shape. The check is a deterministic regex (see
`backend/internal/ai/critique.go`); no extra LLM call.

When the regex matches, the loop appends targeted feedback that quotes the
offending suggested_fix back to the model, lists the exact phrases that
tripped the gate, and re-states the two allowed shapes (concrete remediation
OR the strict no-remediation escape hatch). It then re-prompts; each retry
consumes one extra agentic iteration on top of `max_iters`. Drafts that still
punt after `max_retries` retries are published but NOT cached, so the next
fetcher run retries with a fresh attempt. When enabled, expect 1.0-1.5x
baseline iterations for the typical failure (most analyses pass on the first
try; only the punts incur retries).

**Coverage.** Critique runs both in-loop (on tools-free finals that parse on
the spot, with re-prompt retries) AND post-loop (on outputs from the
force-finalize round when the loop maxed out without finalizing). The
post-loop check is single-shot — it gates caching but doesn't re-prompt — so a
punt-shaped finalize-round result publishes, doesn't cache, and re-analyzes on
the next run.

**Cache invalidation.** Enabling `critique` on an existing project invalidates
any cached entries that didn't pass critique (same two-layer behavior as the
floors). Disabling it does NOT invalidate previously critique-passed entries;
they serve from cache as usual. A `critique_version` int is stamped onto every
critique-passing analysis; the invalidation gates reject entries whose version
is below the current engine's, so strengthening the gate automatically
invalidates entries that passed under the older, weaker contract without
per-consumer cache clears.

#### Hallucinated citation check

Alongside the punt regex, critique runs a deterministic check that rejects a
draft citing an artifact it never read (a confident, fluent root cause built
on an artifact the agent never opened). It combines with the punt check into
one retry message.

The agentic loop records the path of every successful `read_artifact` /
`tail_artifact` / `grep_artifact` call. Critique then scans the draft's
`root_cause`, `summary`, `suggested_fix`, and each `relevant_files` entry for
artifact-shaped tokens (`.log` files plus the known Prow artifacts:
`build-log.txt`, `clone-log.txt`, `started.json`, `finished.json`,
`prowjob.json`, `junit_*.xml`). Source files (`.go`, `.yaml`, generic `.json`)
are excluded because they legitimately live in the source repo, not the
artifact tree. A citation that includes a directory prefix must match a full
read path exactly (catches the cross-machine basename-collision case: reading
`machine-a/boot.log` then citing `machine-b/boot.log` fails). A bare basename
matches any read with the same basename. Failed reads (tool returned
`{"error": ...}`) do NOT count as reads, so a model cannot launder a citation
by reading a non-existent file.

When the loop runs with `critique.enabled: true`, the read-tracking maps are
pre-allocated even before the first successful read, so the check is active
from the first tools-free final. When critique is disabled the maps stay nil
and the check is a free no-op.

#### Skills and recipes

The hallucination check catches structural hallucinations but not semantic
ones (the model reads an artifact and draws the wrong conclusion, e.g. "API
throttling" when the build-log clearly shows x509 errors). Skills add a
consumer-side knowledge layer: each project ships YAML "skills" (recipes) under
`<project_dir>/skills/*.yaml`. When a recipe's trigger regex matches the
model's draft, the critique gate enforces that the agent has read evidence the
recipe declares canonical for the pattern. Missing evidence appends a
per-recipe feedback block (with procedure quoted under a "consumer guidance,
not engine instruction" disclaimer) and dynamically extends the retry budget so
the agent has room to satisfy the missing evidence in the next round.

Skills are not gated by a config flag: shipping recipe files is the opt-in.
They extend the critique gate, so the fetcher auto-enables `critique` when
recipes are present (an explicit `critique` block still supplies
`max_retries`). Every cache entry carries a `skill_set_hash` fingerprint of
the loaded recipe set; consumer edits to any recipe change the hash and
invalidate affected entries on the next run, independently of the
`critique_version` bump.

**Inapplicable recipes do not block caching.** A recipe whose required
evidence does not exist anywhere in the build's artifact tree is inapplicable
to that build: the agent cannot read evidence the run never produced. When a
matched recipe has a missing evidence group, the engine does one bounded
recursive listing of the build tree and drops any group whose `any_of`
patterns match no path in it. Only groups whose evidence **exists but was not
read** remain a genuine miss. The listing is cached per analysis and only
fetched when a skill miss actually occurs; a truncated listing disables the
check (the engine cannot prove a path is absent), preserving the stricter
behavior.

See [`docs/skills.md`](skills.md) for the full schema, authoring guidance, and
observability notes.

### Evidence injection

The critique gate already detects when a draft cites an artifact the agent
never read and re-prompts the model to go read it. Weak models frequently
ignore that instruction and re-emit the same ungrounded claim. When
`evidence_injection` is on, the engine instead **fetches** each cited-but-
unread artifact (the model already named the path), caps it, and embeds its
content directly in the retry feedback: "here is what it actually shows; ground
your root_cause in it or drop the claim." The fetched paths are marked read, so
the next critique pass does not re-flag them.

This converts an ignored "go read X" loop into "here is X", the single most
common reason drafts fail critique on weaker models. It covers two buckets:
artifacts the draft **cited but never read**, and evidence a **matched skill
requires** for the claimed failure class. Full-path citations are fetched
directly; bare-basename citations and skill-required patterns are resolved to
real paths with a single bounded tree walk (so cost does not scale with the
number of targets). It runs on both the in-loop critique retry and the
post-loop force-finalize path (where weak models most often land after
exhausting their tool-call budget), in the latter case driving one extra
finalize round with the injected evidence. If that post-injection finalize
comes back as prose instead of JSON, the engine retries it once before giving
up, so a one-off formatting slip does not discard an otherwise-cacheable
answer. It adds the fetched bytes (up to a few capped artifacts per retry) to
the conversation, so it is best suited to large-context models. Best-effort: a
path that cannot be resolved or fetched is skipped and the plain-text feedback
still applies. No cache-version interaction; it only changes the retry prompt.

### Single tool call

When enabled, the loop sends at most one tool call per assistant turn. Two
mechanisms work together: the request sets the OpenAI
`parallel_tool_calls: false` flag (so endpoints that honor it let the model
pick its single best call at generation time), and as a fallback for endpoints
that ignore the flag, the loop executes and echoes only the first tool call
when several come back at once (the rest are dropped and can be re-requested on
a later turn).

Set this for endpoints whose chat template rejects multiple tool calls in one
assistant message. The stock Llama 3.x Instruct template, for example, raises
`This model only supports single tool-calls at once!` and the provider surfaces
it as a 500 once a multi-tool-call assistant turn is replayed in history. This
is a property of the model's own chat template (the Llama tool-calling format
is one call per turn), not a provider bug, so the fix belongs in the loop.
(Some trtllm/Dynamo builds accept `parallel_tool_calls: false` but ignore it,
which is exactly why the client-side cap is also needed.) Leave it off for
providers that support parallel tool calls so they keep their round-trip
efficiency.

### Cost and behavior

Per failure, agentic analysis uses roughly 50-150k input tokens and runs for
30-90 seconds wall clock. The exact numbers depend on artifact size and how
deep the model digs.

Hitting a byte-budget cap mid-loop triggers a forced finalize round: the
engine drops the `tools` field and asks the model for its final JSON answer
based on whatever it has seen so far. This always produces a usable analysis,
since incomplete is better than absent. Hitting the `timeout`, by contrast,
cancels the in-flight request and the analysis errors out for that failure.

### Parallel analysis

Failures are analyzed sequentially by default, so a full cold-cache fetch takes
roughly `failures x 30-90s`. Each analysis is an independent sequence of model
round-trips, so `concurrency: N` runs up to N investigations at once. A
batching endpoint (self-hosted vLLM / TRT-LLM, which serve many requests on one
GPU via continuous batching) absorbs this and wall-clock drops roughly in
proportion until the endpoint saturates; 4-6 is a good starting point for a
dedicated endpoint.

Defaults to `1` (sequential): the engine has no request-level backoff, so a
shared, rate-limited provider (e.g. GitHub Copilot) can return 429 under
parallelism. The setting is independent of the fetcher's `-workers` flag, which
parallelizes the artifact *fetch* phase, not analysis. Concurrency does not
change results or cache semantics; the AI cache, per-build tool caches, and the
tools-unsupported flag are all internally synchronized.

### Cache semantics

Agentic analyses are cached under `agentic:<module>:<job>:<build>:<hash>`. The
engine records the analysis `mode` on each entry; an entry from a prior
pipeline (or one below the current quality floors) is detected as stale on the
next fetcher run and re-analyzed.

Cached agentic entries are scoped to a specific build because answers cite
build-specific paths and line numbers; the same test failing in two different
builds gets two separate agentic analyses.

## Troubleshooting

- **No analyses are appearing.** Confirm the fetcher ran with `-ai` and
  check the startup logs for `Agentic AI enabled (...)`. A failed tool
  registry enable logs a warning and marks failures unavailable.
- **Every failure logs "AI endpoint rejected tools".** The endpoint
  doesn't support function calling; analyses surface as an "AI analysis
  unavailable" summary. Switch to a function-calling endpoint.
- **Costs spiked.** Lower `max_iters`, or analyze fewer builds. Inspect
  the cached analyses for `mode: "agentic"` to estimate the bill.
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
  module that builds the per-failure seed prompt.
- `backend/internal/artifacts/` — the `Browser` interface and
  `GCSBrowser` implementation backing the filesystem tools.
