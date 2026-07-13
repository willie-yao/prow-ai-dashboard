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

- **Endpoint token cap.** GitHub Models free tier caps each request at 8k (mini) /
  16k (gpt-4o) input tokens. That is smaller than one CAPZ log artifact plus the
  domain prompt, so parity-depth analysis fails with HTTP 413. Use a large-context
  endpoint (Dynamo/Kimi, Copilot, or a paid OpenAI key) for a real evaluation.
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
Dockerfile                 Builds the shim from the backend module.
manifests/
  00-rbac.yaml             Grants the Orka SA full core.orka.ai access (chart gap).
  10-provider.yaml         GitHub Models Provider (swap for a real endpoint).
  20-gcs-tool.yaml         The GCS domain-tool Deployment + Service.
  30-tools.yaml            Filesystem Tool CRDs (list/find/grep/read/tail).
  35-k8s-tools.yaml        k8s discovery Tool CRDs (discover-clusters, find-my-cluster, ...).
  40-example-tasks.yaml    hello-world (Q1) and capz-analyze (Q2) Tasks.
```

The shim itself is `backend/cmd/orka-gcs-tool-spike/main.go`; it must live in the
backend module because it imports the engine's internal tool packages. It
registers both the `filesystem` and `k8s` tool groups over the same artifact
Browser, so a single service backs every Tool CRD above.

## Reproduce

Prerequisites: `kind`, `kubectl`, `helm`, `docker`, and a GitHub PAT with
`models:read`.

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
docker build -f experimental/orka/Dockerfile -t orka-gcs-tool-spike:latest backend/
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

Retrieve a task result. The Orka API validates the bearer via a Kubernetes
TokenReview, so any service-account token works:

```bash
kubectl port-forward -n orka-system svc/orka 18080:8080 &
TOKEN=$(kubectl create token orka -n orka-system)
curl -s -H "Authorization: Bearer $TOKEN" \
  http://localhost:18080/api/v1/tasks/capz-analyze/result
```

Watch the agent's tool calls in the shim logs:

```bash
kubectl logs -n orka-system deploy/gcs-tool -f | grep '🛠'
```

To run with the full CAPZ project prompt, set the `capz-analyze` Task's
`ai.systemPrompt` to the consumer's `prompts/system.md`, and point `BUILD_PREFIX`
in `20-gcs-tool.yaml` at the build you want to analyze.

## Teardown

```bash
kind delete cluster --name orka-spike
```
