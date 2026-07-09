# Serving Kimi-K2 on Ray (KubeRay RayService + vLLM) on AKS

Replaces the NVIDIA Dynamo serving layer with a **KubeRay RayService** running
**Ray Serve LLM (vLLM)**. The dashboard only needs an OpenAI-compatible
`/v1/chat/completions` endpoint, so this is a drop-in swap: stand up the
RayService, then repoint the dashboard's `AI_ENDPOINT`.

`deploy/ray/rayservice-kimi-k2.yaml` is a faithful port of the live DGD
`kimi-k27-code-agg`: model `moonshotai/Kimi-K2.7-Code`, **TP=16 across 2 nodes**,
256K context, `runai_streamer` load from the Lustre cache, InfiniBand RDMA. It
also leans into a few Ray-native strengths (declarative autoscaling, deploy-time
deps, zero-downtime upgrades) — see [dynamo-vs-ray.md](dynamo-vs-ray.md) for the
full UX comparison.

## What already exists on the `h100` cluster (reused as-is)
The platform stack is framework-agnostic and stays in place:
- GPU Operator (drivers, container toolkit, device plugin)
- NVIDIA Network Operator + MOFED + `rdma-shared-dp` (InfiniBand / RDMA)
- Azure Managed Lustre PVC **`pvc-lustre`** (16Ti RWX) holding the model weights
- Secret **`hf-token-secret`** (HF token)
- NVSentinel (GPU health), NFD, monitoring

Only **KubeRay + the RayService** are new. The 4 H100 nodes give 32 GPUs;
Dynamo's vLLM workers sit on **node0 + node1** (16 GPUs), leaving **node2 +
node3 free** (16 GPUs). The RayService uses pod anti-affinity to land its two
workers on those free nodes, so Ray and Dynamo run **side by side** for a direct
A/B comparison before you cut over.

## Steps

### 1. Point kubectl at the cluster
```bash
az account set --subscription ff05f55d-22b5-44a7-b704-f9a8efd493ed
az aks get-credentials -g h100 -n h100 --overwrite-existing
```

### 2. Install the KubeRay operator (adds the RayService/RayCluster CRDs)
```bash
helm repo add kuberay https://ray-project.github.io/kuberay-helm/ && helm repo update
helm install kuberay-operator kuberay/kuberay-operator \
  -n kuberay --create-namespace --version 1.4.0
kubectl get crd | grep ray.io
```

### 3. Apply the RayService
```bash
kubectl apply -f deploy/ray/rayservice-kimi-k2.yaml
```
It reuses `pvc-lustre` and `hf-token-secret` already in the `default` namespace.
KubeRay reconciles it into a RayCluster (1 CPU head on `nodepool1` + 2 GPU
workers on `ndh100pool`), then Ray Serve LLM boots vLLM with a 16-way
tensor-parallel group spanning both worker pods over InfiniBand. Pod
anti-affinity keeps the Ray workers on the two H100 nodes Dynamo is **not**
using (node2 + node3), so both stacks coexist.
```bash
kubectl get rayservice kimi-k2 -w
kubectl get pods -l ray.io/cluster -o wide
```
Weights load from the Lustre cache (`runai_streamer`), so startup is fast once
GPUs are allocated.

### 4. Test the endpoint
```bash
kubectl port-forward svc/kimi-k2-serve-svc 8000:8000 &
curl http://localhost:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"moonshotai/Kimi-K2.7-Code","messages":[{"role":"user","content":"hi"}]}'
```

### 5. Repoint the dashboard at Ray
The worker in `capz-dynamo` currently targets the Dynamo frontend:
```
AI_ENDPOINT=http://kimi-k27-code-agg-frontend.default.svc.cluster.local:8000/v1/chat/completions
```
Switch it to the Ray Serve service (same `AI_MODEL`):
```
AI_ENDPOINT=http://kimi-k2-serve-svc.default.svc.cluster.local:8000/v1/chat/completions
AI_MODEL=moonshotai/Kimi-K2.7-Code
```
Update it in the dashboard's Helm values / deployment env and roll the worker.
Clear the AI cache after switching (the project's `clear-cache.yml`) so cached
analyses re-run against Ray.

### 6. (Optional) Decommission Dynamo
Once Ray serves traffic, free the other 16 GPUs:
```bash
kubectl delete dynamographdeployment kimi-k27-code-agg -n default
```

## How the Ray config maps to the DGD

| DGD (`kimi-k27-code-agg`) | RayService (`kimi-k2`) |
|---|---|
| `backendFramework: vllm` + `dynamo.frontend` | `ray.serve.llm:build_openai_app` (frontend + vLLM) |
| `--tensor-parallel-size 16`, `multinode.nodeCount: 2` | `engine_kwargs.tensor_parallel_size: 16` + `distributed_executor_backend: ray`, 2 worker pods |
| `--max-model-len 262144` | `engine_kwargs.max_model_len: 262144` |
| `--max-num-batched-tokens 16384` | `engine_kwargs.max_num_batched_tokens: 16384` |
| `--gpu-memory-utilization 0.9` | `engine_kwargs.gpu_memory_utilization: 0.9` |
| `--load-format runai_streamer` | `engine_kwargs.load_format: runai_streamer` |
| `--dyn-tool-call-parser kimi_k2` | `tool_call_parser: kimi_k2` + `enable_auto_tool_choice` |
| `envFromSecret: hf-token-secret` | `envFrom: secretRef hf-token-secret` |
| `pvc-lustre` @ `/mnt/model-cache`, `HF_HUB_OFFLINE=1` | same volume + env |
| `rdma/hca_shared_devices_a: 1`, IB env, `IPC_LOCK`/`SYS_RESOURCE`, shm 256Gi | same |
| gpu: 8 per worker, `agentpool: ndh100pool` | same |

## Caveats
- **Image parity.** The DGD uses NVIDIA's custom `vllm-runtime:1.3.0-kimi-k2.6`
  image, which bundles `runai_streamer` and the `kimi_k2` parser. The stock
  `rayproject/ray-llm` image may not. If startup fails on `load_format` or
  `tool_call_parser`, either install `runai-model-streamer` via the app's
  `runtime_env.pip` (commented in the manifest) and confirm your vLLM version
  ships the Kimi parser, or build a Ray image `FROM` the NVIDIA runtime.
- **Multi-node placement.** `distributed_executor_backend: ray` is what lets the
  16-GPU TP group span both worker pods. If it schedules onto one node and
  fails, verify both `ndh100pool` workers are Ready and the placement-group
  strategy allows spanning nodes.
- **Scheduling.** Dynamo used Grove + KAI scheduler for gang scheduling. KubeRay
  brings up the RayCluster itself; KAI/Kueue integration is optional and not
  required for this single deployment.
