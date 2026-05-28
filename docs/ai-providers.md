# AI providers

The dashboard's AI analysis is provider-agnostic. The fetcher speaks plain
OpenAI chat-completions over HTTPS, so anything that exposes a
`POST /chat/completions` endpoint will work: GitHub Copilot, OpenAI, Azure
OpenAI, Nvidia Dynamo / NIMs, vLLM, Ollama, or a self-hosted proxy.

Configure your provider in your consumer repo's `project.yaml` under `ai:`:

```yaml
ai:
  module: "capi"          # Required. Selects the prompt & evidence module.
  endpoint: "..."         # Optional. Chat-completions URL. Defaults to Copilot.
  model: "..."            # Optional. Model identifier the endpoint expects.
  headers:                # Optional. Extra HTTP headers merged into every call.
    Some-Header: "value"
```

Set the bearer token via the `AI_TOKEN` secret in the GitHub Actions workflow
(see the [reusable workflow README](../README.md)). The token is sent as
`Authorization: Bearer <AI_TOKEN>` unless an entry in `headers:` overrides it.

## GitHub Copilot (default)

This is what you get if you leave `endpoint` and `model` unset. Token is
your fine-grained PAT with the `copilot_chat` user permission.

```yaml
ai:
  module: "capi"
  endpoint: "https://api.githubcopilot.com/chat/completions"
  model: "claude-opus-4.7-xhigh"
```

Models verified against the Copilot endpoint:

- `claude-opus-4.7-xhigh` — extra-high reasoning effort, 200K context.
  Best quality for debugging analysis. Engine default. Marked
  "(Internal only)" in the agent model list but currently accepted by
  the public endpoint; treat availability as best-effort.
- `claude-opus-4.7` — newer model, standard reasoning, 200K context.
  Use as the fallback if xhigh access is revoked.
- `claude-opus-4.6` — stable, broadly available. Safe baseline if you
  hit access issues with the 4.7 line.
- `claude-opus-4.7-1m-internal` — 1M context, standard reasoning. Only
  useful if prompts exceed ~150K tokens; standard prompts don't need it.

There is no combined "1M + xhigh" variant.

The fetcher automatically sends `Copilot-Integration-Id: copilot-developer-cli`
when (and only when) the endpoint's host is `*.githubcopilot.com`.

## OpenAI

```yaml
ai:
  module: "capi"
  endpoint: "https://api.openai.com/v1/chat/completions"
  model: "gpt-4o"
```

`AI_TOKEN` is your OpenAI API key.

## Azure OpenAI

Azure OpenAI uses a per-deployment URL and an `api-key` header instead of
`Authorization: Bearer`. Put the key in the `headers:` map so it replaces
the default bearer scheme:

```yaml
ai:
  module: "capi"
  endpoint: "https://my-resource.openai.azure.com/openai/deployments/gpt-4o/chat/completions?api-version=2024-08-01-preview"
  model: "gpt-4o"
  headers:
    api-key: "${AI_TOKEN}"
```

Note: `${AI_TOKEN}` interpolation isn't built in. Either inject the literal
value via a workflow `env:` step or set the header directly in the YAML
(only safe for non-secret routing values).

## Nvidia Dynamo / NIM

NIMs accept the OpenAI schema. Use the model name your NIM exposes:

```yaml
ai:
  module: "capi"
  endpoint: "https://integrate.api.nvidia.com/v1/chat/completions"
  model: "meta/llama-3.1-70b-instruct"
```

`AI_TOKEN` is your NVIDIA API key. For self-hosted NIMs, point `endpoint` at
your cluster's gateway and add any routing headers your gateway expects.

## vLLM / Ollama / self-hosted

Any OpenAI-compatible server works. For Ollama:

```yaml
ai:
  module: "capi"
  endpoint: "http://localhost:11434/v1/chat/completions"
  model: "llama3.1"
```

Self-hosted endpoints typically don't require a token; set `AI_TOKEN` to any
non-empty placeholder in your workflow so the env check in the fetcher passes.

## Cache invalidation when switching providers

Cache keys are content-based (hash of the test name + normalized failure
message) and do not include the model or endpoint. Switching providers will
return stale cached responses from the previous model until the cache is
cleared. Run the project's `clear-cache.yml` workflow after changing
`endpoint` or `model` if you want fresh analyses.

Switching `ai.agentic.enabled` on or off does NOT need a cache clear:
the engine records the analysis mode (`curator` or `agentic`) on each
cached entry and re-analyzes the failure when the cached mode no longer
matches the desired mode. Switching mode therefore self-heals over one
fetcher run.

## Function-calling support (required for agentic mode)

Agentic mode (see [agentic.md](agentic.md)) sends an OpenAI-style
`tools` field on every request and expects `tool_calls` back from the
model. The engine probes lazily: the first agentic call to an endpoint
that returns HTTP 400/422 with a tools-related error is treated as a
capability miss; the fetcher falls back to the curator pipeline for
the rest of that run and logs `AI endpoint rejected tools`. Verified
endpoints: GitHub Copilot, OpenAI, Azure OpenAI, and tool-calling
Ollama / NIM models (per-model). Curator mode has no function-calling
requirement.

## Cost and latency notes

Each non-transient failure triggers one chat-completion call in
curator mode (the default). The regex transient-failure triage in each
module runs first and is free, so flaky runs (Azure throttling,
image-pull retries, etc.) skip the model entirely.

Token use per call (curator mode): roughly 3-15k input tokens
(depending on how much debug-log evidence the module ships) and
200-800 output tokens. Most providers price the input dominant.

Agentic mode uses roughly 50-150k input tokens and 30-90 seconds of
wall clock per failure (vs 5-15s for curator). Enable it selectively;
see [agentic.md](agentic.md) for cost-control knobs.
