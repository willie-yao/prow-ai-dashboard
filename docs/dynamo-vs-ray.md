# Dynamo vs. Ray Serve LLM: an operator's UX comparison

Notes from porting the live `kimi-k27-code-agg` DGD to a KubeRay RayService
(`deploy/ray/rayservice-kimi-k2.yaml`). Both serve the same model, over the same
GPUs, behind the same OpenAI endpoint — so this is a clean apples-to-apples look
at the two developer/operator experiences.

## TL;DR

- **Dynamo** is a *specialized LLM inference system*. Its UX is organized around
  the LLM serving graph (Frontend / Router / Worker, prefill/decode
  disaggregation, KV-aware routing). You get high-end serving features as
  first-class spec fields, at the cost of a heavier platform (operator + NATS +
  Grove + a scheduler) and a narrower, LLM-only scope.
- **Ray Serve LLM** is a *general serving framework with an LLM module*. Its UX
  is organized around Ray primitives (deployments, replicas, placement groups,
  runtime envs). You get flexibility, simple autoscaling, deploy-time dependency
  management, and zero-downtime upgrades, but multi-node model parallelism and
  advanced KV features are more "assemble it yourself" than turnkey.

## Architecture philosophy

| | Dynamo | Ray Serve LLM |
|---|---|---|
| What it is | Purpose-built LLM serving stack | General distributed-app framework + LLM library |
| Unit of deployment | `DynamoGraphDeployment` (a graph of components) | `RayService` -> RayCluster + Serve apps |
| Control plane | `dynamo-operator` + NATS + Grove (+ KAI scheduler) | KubeRay operator + Ray head (GCS) |
| Scope | LLM inference only | Any Python workload; LLM is one module |
| Mental model | "Describe my serving graph" | "Run my Serve app on a Ray cluster" |

## Config authoring UX

Both are declarative YAML. The difference is *what* you describe.

- **Dynamo**: you author **components** (`Frontend`, `VLLMWorker`) and wire vLLM
  through raw CLI args (`--tensor-parallel-size 16`, `--load-format ...`). The
  multinode worker is a first-class concept (`multinode.nodeCount: 2`) and the
  frontend's KV router is one flag (`--router-mode kv`). It reads like "operate
  this inference topology."
- **Ray**: you author an **app** (`build_openai_app`) with structured
  `llm_configs` (`model_loading_config`, `engine_kwargs`, `deployment_config`).
  vLLM knobs are typed keys instead of a CLI string. Multi-node TP is implied by
  `tensor_parallel_size` + `distributed_executor_backend: ray` and Ray placement
  groups, rather than an explicit node count. It reads like "configure this
  deployment."

Net: Dynamo's YAML is more *serving-topology aware*; Ray's is more *structured
and general*. For someone who knows vLLM CLI flags, Dynamo feels direct; for
someone who wants typed config and app semantics, Ray feels cleaner.

## Where Dynamo is stronger

- **Disaggregated serving (prefill/decode split)** as a first-class feature,
  with **NIXL** KV-cache transfer and **KV-aware smart routing**. This is
  Dynamo's headline capability and is genuinely hard to replicate.
- **LLM-tuned defaults**: KV router, worker metadata, model-download job,
  SLA-aware Planner autoscaling — all built for inference.
- **Multinode workers are turnkey**: `multinode.nodeCount` + Grove gang
  scheduling handle the leader/worker choreography.

## Where Ray is stronger (and what the manifest leans into)

- **Autoscaling UX**: request-load autoscaling is a few lines in
  `deployment_config.autoscaling_config` (`target_ongoing_requests`,
  `min/max_replicas`). Dynamo needs the separate Planner. Ray also supports
  **scale-to-zero** (`min_replicas: 0`) — compelling for this dashboard, which
  only calls the model on a cron: idle between fetches could free 16 H100s.
- **Deploy-time dependency management** (`runtime_env`): add
  `runai-model-streamer` or any pip dep without building a custom image. Dynamo
  bakes everything into `vllm-runtime:...-kimi-k2` images you must maintain.
- **Zero-downtime upgrades**: editing `serveConfigV2` triggers a RayService
  rolling upgrade (new RayCluster, health-checked cutover). Model/arg changes
  don't drop traffic.
- **Observability out of the box**: the Ray Dashboard (`:8265`) shows
  deployments, replicas, placement groups, and logs; Serve exposes Prometheus
  metrics (`:8080`) that plug into the cluster's existing kube-prometheus stack.
- **Generality**: the same cluster/framework can host embeddings, batch jobs,
  pre/post-processing, or non-LLM services beside the model — no new platform.

## Day-2 operations

| Task | Dynamo | Ray |
|---|---|---|
| Install footprint | operator + NATS + Grove + scheduler | KubeRay operator only |
| Change model args | edit DGD, operator reconciles | edit `serveConfigV2`, rolling upgrade |
| Add a Python dep | rebuild + push image | `runtime_env.pip` (or rebuild) |
| Autoscale on load | Planner component | `autoscaling_config` |
| Inspect internals | component pods + NATS | Ray Dashboard + Serve UI |
| Multi-node parallel | `multinode.nodeCount` (turnkey) | placement group + `distributed_executor_backend: ray` |
| Disaggregated serve | first-class | not out of the box |

## What stays identical (proof it's a clean swap)

Both run on the *same* substrate, untouched by the choice: the H100 node pool,
GPU Operator, NVIDIA Network Operator / InfiniBand, the `pvc-lustre` weight
cache, the `hf-token-secret`, NVSentinel health, and monitoring. Only the
serving layer differs, and the dashboard only sees an endpoint URL change
(`kimi-k27-code-agg-frontend` -> `kimi-k2-serve-svc`).

## Verdict / when to pick which

- **Pick Dynamo** when peak inference efficiency matters: high QPS, long
  contexts, and you want prefill/decode disaggregation + KV-aware routing
  without building it yourself. You accept a heavier, LLM-specific platform.
- **Pick Ray Serve LLM** when you value operational simplicity and flexibility:
  fewer moving parts, declarative autoscaling (incl. scale-to-zero), no-rebuild
  dependency changes, zero-downtime upgrades, first-class observability, and the
  freedom to run non-LLM workloads on the same cluster. You give up turnkey
  disaggregation.

For this dashboard's traffic pattern (bursty, scheduled, single model), Ray's
autoscaling + scale-to-zero + simpler ops are a strong fit; Dynamo's
disaggregation advantages mostly show up under sustained high load.
