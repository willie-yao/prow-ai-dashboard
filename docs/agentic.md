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
```

Defaults match the spike that validated the design and are conservative
enough that you almost never need to tune them. Lower `max_iters` first
if you see the model loop without converging; raise `gcs_byte_budget` if
your builds have very large logs and grep is being cut short.

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
