# Orka experiment (isolated)

This directory holds a self-contained experiment that runs the dashboard's AI
analysis through [Orka](https://github.com/orka-agents/orka), a Kubernetes-native
agent orchestration platform, instead of the engine's built-in agentic loop. It
lives on the `orka` branch and touches nothing in `main`.

> **Temporary / experimental.** Everything in `experimental/orka/` and
> `backend/cmd/orka-gcs-tool-spike/` exists only on the `orka` branch to support
> this evaluation. None of it is wired into any deploy, the Dockerfile, CI, or
> `main`. Delete both when the evaluation concludes or if Orka is not adopted; no
> production code depends on them.

The question being evaluated: could Orka do "all the dashboard info without any
custom agent code"?

## Findings summary

Full write-up: the session's `orka-evaluation-spike.md`. Short version:

- **Model access works.** An Orka Provider against an OpenAI-compatible endpoint
  drives a `type: ai` Task end to end.
- **GCS tools work.** The engine's real filesystem tools, exposed over HTTP by the
  shim here, let an Orka agent run a genuine multi-round investigation and produce
  a correct, artifact-grounded root cause. No custom orchestration code.
- **Reaching the engine's depth is the hard part.** "No agent code" is not "no
  code": you still run the GCS domain tool service, an ingestion shim back to
  `dashboard.json`, and the frontend. The engine's quality machinery has no direct
  Orka equivalent and is load-bearing: the `min_tool_calls` / `min_gcs_bytes`
  floors, the deterministic critique gate, the skills registry, and the
  `k8s.discover_clusters` tool tier. Orka's `RepositoryScan.validationMode` is only
  a coarse analog.

### Known constraints (read before re-running)

- **Endpoint token cap (use Copilot to avoid it).** GitHub **Models** free tier
  (`models.github.ai`) caps each request at 8k (mini) / 16k (gpt-4o) input tokens,
  smaller than one CAPZ log artifact plus the domain prompt, so parity-depth
  analysis fails with HTTP 413. GitHub **Copilot** (`api.githubcopilot.com`) is a
  different product with your subscription's full context window. Prefer the
  Copilot provider (`11-copilot-provider.yaml` + the proxy) for real runs; see
  "Using GitHub Copilot" below.
- **Iteration budget.** Orka's ai loop defaults to 10 rounds and is only raised via
  autonomous coordination mode, not a Task field. The engine used 21 tool calls.
- **SSRF guard.** Orka's Tool controller marks any tool whose URL resolves to a
  private/loopback IP as `Available=false`. In-cluster ClusterIPs are private, so
  the tools here show unavailable but still work (see `manifests/30-tools.yaml`).
- **Cluster mapping.** A CAPZ e2e build runs many specs, each with its own cluster.
  The filesystem-only agent could not reliably map a failed test to its cluster.
  The k8s discovery tier (`35-k8s-tools.yaml`, `discover-clusters` /
  `find-my-cluster`) is now exposed to close this gap: it surfaces the candidate
  clusters so the agent stops guessing. Note `find-my-cluster`'s heuristic does not
  match every test name; when it returns no match it still returns the candidate
  list for the agent to reason over.

## Layout

```
Dockerfile                 Builds either shim from the backend module (CMD build-arg).
manifests/
  00-rbac.yaml             Grants the Orka SA full core.orka.ai access (chart gap).
  10-provider.yaml         GitHub Models Provider (free, small per-request token cap).
  11-copilot-provider.yaml GitHub Copilot Provider via the proxy (full context window).
  20-gcs-tool.yaml         The GCS domain-tool Deployment + Service.
  30-tools.yaml            Filesystem Tool CRDs (list/find/grep/read/tail).
  35-k8s-tools.yaml        k8s discovery Tool CRDs (discover-clusters, find-my-cluster, ...).
  40-example-tasks.yaml    hello-world (Q1) and capz-analyze (Q2) Tasks.
  50-copilot-proxy.yaml    Header-injecting proxy so Orka can reach Copilot.
```

Two shims live under `backend/cmd/` (they must be in the backend module to import
the engine's internal packages):
- `orka-gcs-tool-spike` registers the `filesystem` and `k8s` tool groups over one
  artifact Browser, so a single service backs every Tool CRD above.
- `orka-copilot-proxy` injects the `Copilot-Integration-Id` header Orka cannot set
  itself, so an `openai`-type Provider can reach `api.githubcopilot.com`.

## Reproduce

Prerequisites: `kind`, `kubectl`, `helm`, `docker`, and a GitHub PAT. Use a
`models:read` PAT for GitHub Models, or a `copilot_chat` PAT for Copilot.

```bash
# 1. Cluster
kind create cluster --name orka-spike

# 2. Install Orka. Orka's published images are private, so build from source:
git clone https://github.com/orka-agents/orka /tmp/orka-src && cd /tmp/orka-src
docker build -t ghcr.io/orka-agents/orka:latest .
docker build -t ghcr.io/orka-agents/orka/ai-worker:latest -f workers/ai/Dockerfile .
kind load docker-image ghcr.io/orka-agents/orka:latest --name orka-spike
kind load docker-image ghcr.io/orka-agents/orka/ai-worker:latest --name orka-spike
helm install orka charts/orka -n orka-system --create-namespace
kubectl apply -f config/crd/bases/         # chart does not bundle CRDs
cd -

# 3. Back in this repo: RBAC gap fix + provider secret
kubectl apply -f experimental/orka/manifests/00-rbac.yaml
kubectl rollout restart deploy orka-controller -n orka-system
kubectl create secret generic gh-models-secret -n orka-system \
  --from-literal=api-key=<YOUR_GITHUB_PAT>

# 4. Build + load the GCS tool shim
docker build -f experimental/orka/Dockerfile --build-arg CMD=orka-gcs-tool-spike \
  -t orka-gcs-tool-spike:latest backend/
kind load docker-image orka-gcs-tool-spike:latest --name orka-spike

# 5. Apply provider, tool service, tools
kubectl apply -f experimental/orka/manifests/10-provider.yaml
kubectl apply -f experimental/orka/manifests/20-gcs-tool.yaml
kubectl apply -f experimental/orka/manifests/30-tools.yaml
kubectl apply -f experimental/orka/manifests/35-k8s-tools.yaml

# 6. Wait for the Provider to be Ready, then run the tasks
kubectl wait provider/gh-models -n orka-system --for=jsonpath='{.status.ready}'=true
kubectl apply -f experimental/orka/manifests/40-example-tasks.yaml
kubectl get task -n orka-system -w
```

### Using GitHub Copilot instead of GitHub Models

GitHub Models and GitHub Copilot are different products. Models is a free
catalog with a small per-request token cap; Copilot is your subscription with a
full context window. For parity-depth analysis, use Copilot: it needs a
`Copilot-Integration-Id` header that Orka's Provider cannot set, so requests go
through the header-injecting proxy in `50-copilot-proxy.yaml`.

```bash
# Secret: a fine-grained PAT with the copilot_chat permission.
kubectl create secret generic copilot-secret -n orka-system \
  --from-literal=api-key=<YOUR_COPILOT_CHAT_PAT>

# Build + load the proxy (same Dockerfile, different CMD build-arg).
docker build -f experimental/orka/Dockerfile --build-arg CMD=orka-copilot-proxy \
  -t orka-copilot-proxy:latest backend/
kind load docker-image orka-copilot-proxy:latest --name orka-spike

# Deploy the proxy + Copilot provider.
kubectl apply -f experimental/orka/manifests/50-copilot-proxy.yaml
kubectl apply -f experimental/orka/manifests/11-copilot-provider.yaml
kubectl wait provider/copilot -n orka-system --for=jsonpath='{.status.ready}'=true

# Point a Task at it: set ai.providerRef.name to "copilot" and use a Copilot
# model (e.g. claude-sonnet-4.5) in the Task's ai.model.
```

Retrieve a task result. The Orka API validates the bearer via a Kubernetes
TokenReview, so any service-account token works:

```bash
kubectl port-forward -n orka-system svc/orka 18080:8080 &
TOKEN=$(kubectl create token orka -n orka-system)
curl -s -H "Authorization: Bearer $TOKEN" \
  http://localhost:18080/api/v1/tasks/capz-analyze/result
```

Watch the agent's tool calls in the shim logs, or the proxy's forwarded requests:

```bash
kubectl logs -n orka-system deploy/gcs-tool -f | grep '🛠'
kubectl logs -n orka-system deploy/orka-copilot-proxy -f
```

To run with the full CAPZ project prompt, set the `capz-analyze` Task's
`ai.systemPrompt` to the consumer's `prompts/system.md`, and point `BUILD_PREFIX`
in `20-gcs-tool.yaml` at the build you want to analyze.

## Teardown

```bash
kind delete cluster --name orka-spike
```
