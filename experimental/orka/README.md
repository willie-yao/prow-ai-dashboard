# Orka backend (experimental)

Runs the dashboard's AI failure analysis through
[Orka](https://github.com/orka-agents/orka), a Kubernetes-native agent
orchestration platform, instead of the engine's in-process agentic loop. Discovery
and output are unchanged; only the per-failure analysis step moves to Orka Tasks
that run alongside your inference stack.

> Experimental, `orka` branch only. Nothing here is wired into `main`, CI, or the
> product image. No production code depends on it.

## Docs

- **[USAGE.md](USAGE.md)** - how to deploy and run the Orka path, the config it
  reads, and the knobs. Start here.
- **[MIGRATION.md](MIGRATION.md)** - the evaluation (what was proven, the
  same-model control, the honest cost/quality trade) and the productization
  recommendation.
- **[worker-patches/](worker-patches/)** - the required Orka ai-worker changes
  (convergence + transient-critique) and the numbers behind them.

## Headline finding

Orka + a strong model (claude-sonnet-4.5) produces excellent, artifact-grounded
analyses that match or beat the engine's reference labels on the hardest CAPZ
cases. But that edge is the MODEL, not the harness: on the engine's own cheap model
(gemini-3.5-flash) Orka is no better at classification, and it needed the worker
patches just to converge. After those patches Orka is at process parity with the
engine on a cheap model (converges 15/15, grounds every transient verdict), and the
residual quality gap is pure model capability. Recommendation: adopt Orka as an
optional, co-located, strong-model backend; keep the engine as the default
cheap-model path; do not decommission. Details and numbers in MIGRATION.md.

## Known constraints

- **Copilot needs the de-streaming proxy.** Copilot's non-streaming endpoint
  returns null tool_calls for Claude (the calls only arrive over streaming SSE).
  manifests/50-copilot-proxy.yaml de-streams so the worker sees real tool calls.
  Standard OpenAI-compatible endpoints (vLLM, Ray Serve, Dynamo/NIM, Ollama) do NOT
  need it - point a Provider straight at them.
- **Worker patches are required.** Small models otherwise fail to converge (submit
  empty results) and skip the verify_timeline discipline. Apply
  worker-patches/ai-worker-convergence.patch (iteration cap, forced finalization,
  empty-final re-prompt, transient-critique gate) and rebuild the ai-worker image.
- **SSRF guard.** Orka marks tools whose URL resolves to a private/loopback IP as
  Available=false. In-cluster ClusterIPs are private, so the tools show unavailable
  but still work (the worker does not gate on availability).
- **Cluster mapping (CAPZ).** A CAPZ e2e build runs many specs, each with its own
  cluster. The k8s tool group (35-k8s-tools.yaml, discover-clusters /
  find-my-cluster) lets the agent map a failing test to its cluster; enable it via
  ai.tools: [filesystem, k8s]. Filesystem-only consumers (e.g. non-CAPI projects)
  set ai.tools: [filesystem].

## Teardown

```bash
kind delete cluster --name orka-spike
```
