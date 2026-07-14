# Orka migration plan (on the `orka` branch)

Goal: produce a real dashboard for one consumer (capz) end to end through Orka,
on the `orka` branch, without rewriting the parts that already work.

Status: M0-M5 complete. The pipeline runs end to end (fetcher -> producer ->
Orka Tasks -> ingestor) and patches real capz failures into the dashboard. M5's
self-critique prompt was validated on a live task: it converged (no max-iters)
and produced grounded, build-correct JSON.

## Key simplifier: reuse the fetcher, move only the AI step

The current engine is: discover jobs/builds/failing tests -> analyze each failure
with the in-process agentic loop -> write dashboard.json + jobs/*.json -> frontend
serves them. Only the ANALYSIS step moves to Orka. Discovery and output stay.

So the migration is: run the fetcher with AI DISABLED to write the dashboard
SKELETON (all jobs/builds/tests, no AI), have a producer create one Orka Task per
failure, and have an ingestor patch the Orka analysis back into the jobs/*.json.
Frontend is unchanged.

```
fetcher (-ai=false)  ->  dashboard.json + jobs/*.json  (skeleton, works today)
producer             ->  one content-addressed Orka Task per failing test
Orka + Copilot + GCS tools  ->  per-failure analysis JSON in the result store
ingestor             ->  patch ai_summary/ai_analysis into jobs/*.json
frontend             ->  unchanged
```

## Phases

### M0 - Multi-build GCS tool service (FEASIBILITY GATE)

Today the shim is pinned to one build via BUILD_PREFIX. A real dashboard analyzes
many builds concurrently, so the shared tool service must serve the RIGHT build
per Task. Orka's HTTP tool executor sends the model's params as the JSON body and
interpolates `{{param}}` in the URL, but does NOT auto-forward task identity.

Two viable designs:
- (D) Build param: model passes `build` on every tool call. PROTOTYPED AND
  REJECTED: the model does not reliably pass it, and a silent fallback to the
  default build produced confidently-wrong analyses (a security-group
  investigation on an azl3 failure). Fail-wrong is unacceptable for a dashboard.
- (L, CHOSEN) Per-build Tool CRDs: for each distinct build the producer clones
  the base Tool CRDs with a static `X-Build-Prefix: <prefix>` header and a
  build-suffixed name; each Task references its build's tool set. The shim (M0
  multi-build) routes by that header, so tools always read the Task's own build
  regardless of the model. Cost: base-tools x distinct-builds CRDs (e.g. 16 x 4 =
  64 for the demo), created and GC'd by the producer/orchestrator.

M0 shim is done (serves any build via the `build` body param OR the
`X-Build-Prefix` header). L is validated end to end: four concurrent Tasks each
read their OWN build's clusters and produced build-correct analyses.

### M1 - Fetcher no-AI skeleton (low risk, no new code)

Run the existing fetcher with `-ai=false` (or ai disabled) against the capz
consumer to produce dashboard.json + jobs/*.json with all jobs/builds/tests and
no AI. Confirms the skeleton path and gives the ingestor a target to patch.

### M2 - Producer (failures -> Orka Tasks)

New `backend/cmd/orka-producer`: load the consumer project.yaml
(`internal/project`), enumerate recent builds + failing tests
(`internal/prowbuild`, `internal/junit`), compose the prompt
(`internal/ai` BasePrompt + consumer system.md + ResponseFormatFooter via
`compose`), and create one `type: ai` Task per failure. Content-addressed
run-once: name = `az-<job>-<build>-<failureHash>-v<N>`; creating an existing name
is a no-op (the K8s object store is the cache). Tasks reference the multi-build
GCS tools + the quality tools, providerRef=copilot (or dynamo later).

### M3 - Ingestor (results -> dashboard)

New `backend/cmd/orka-ingestor`: list completed `az-*` Tasks, fetch each result,
parse the analysis JSON, map to `models.AISummary` / `models.AIAnalysis`, and
patch them into the matching test case in jobs/*.json (reuse `internal/output`
writers + `internal/models`). Idempotent; safe to re-run as Tasks complete.

### M4 - Orchestrate + deploy on the branch

`experimental/orka/run-demo.sh <consumer-dir>` runs the whole pipeline against the
`orka-spike` cluster: fetcher(no-AI) -> producer -> apply Tools+Tasks -> poll ->
ingestor -> a renderable data dir. Validated end to end (patched 4/4 failing tests
into a real capz dashboard). A fully in-cluster CronJob (containerized producer +
ingestor with RBAC) is the hardening step after this local demo.

### M5 - Quality contract in the Task (from A2 findings)

Bake into the Task systemPrompt: a reviewer/self-critique step, a
"confirm a transient claim with verify_timeline before setting is_transient=true"
rule, and "default to bug unless a transient class is proven" (the engine's
contract). Keeps classification comparable-or-better than the engine.

## Sequencing / risks

- M1 is independent and works today. M0 gates M2/M3. M2 -> M3 -> M4. M5 refines M2.
- Risks: M0 model-dependence (mitigation L); Orka experimental constraints (SSRF
  guard, 10-iter default - both already worked around here); Copilot rate limits
  at batch scale (mitigation: point the Provider at the in-cluster Dynamo/Kimi
  stack). None are blockers proven in the spike.
- Everything stays on the `orka` branch under `experimental/orka/` +
  `backend/cmd/orka-*`; the engine core and `main` are untouched.

## Not in scope (yet)

Event-driven producer (GCS watcher) instead of cron; native RepositoryScan-style
remediation (issues/fix-PRs); tearing out the engine's in-process AI. Those come
after a working end-to-end batch demo.

## Hardening roadmap (make it a real in-cluster deploy, not a laptop script)

Today the pipeline runs from `run-demo.sh` on a laptop against the `orka-spike`
kind cluster, polling for results, with Copilot behind a de-streaming proxy. The
target is an in-cluster deploy that slots into the existing Helm chart
(`deploy/helm/prow-ai-dashboard`): fetcher/producer/ingestor share the chart's
ReadWriteMany PVC at `/data`, and the existing server Deployment serves the
Orka-produced JSON with no frontend change.

### H1 - Containerize producer + ingestor
Add `orka-producer` + `orka-ingestor` to the image build (extend the Dockerfile
or a separate orka image target). Both build clean today. Low risk.

### H2 - In-cluster run-once Job (RBAC, port run-demo.sh)
Port the demo script into a Job/CronJob against the shared PVC: fetcher(-ai=false)
-> producer -> apply Tasks+Tools -> poll -> ingestor. Add a ServiceAccount +
Role/RoleBinding for create/get/list on `tasks.core.orka.ai` + `tools.core.orka.ai`;
the SA token doubles as the result-API bearer (TokenReview accepts any SA token).
OPEN DECISION: producer/ingestor create objects via client-go (adds a dependency
the engine deliberately avoided, so guard it to the orka cmds) vs a
kubectl-bearing image that applies the emitted YAML. Ingestion still polls here.

DECIDED: client-go, guarded to the orka cmds (backend/cmd/orka-* + a small
orkamig k8s client helper); the engine core stays client-go-free. Ingestion
still polls here.

### H3 - Event-driven ingestion via Task.webhookURL
The Task CRD supports `webhookURL`. Producer sets it on each Task pointing at a
long-lived `orka-ingestor` Service that parses the result and patches the PVC;
the producer becomes a fast fire-and-forget CronJob. RESPECT the chart's
single-writer invariant: the fetcher writes the skeleton, then the ingestor is
the sole patcher (atomic per-file rewrite or a lock).

### H4 - Task lifecycle: retries, failure surfacing, Tool GC
Use the Task `retryPolicy` for transient model/tool errors. The ingestor surfaces
a Failed / max-iters Task as analysis-unavailable on that test (honest dashboard,
never a silent skip). GC per-build Tool CRDs (label by build/run and delete once
that build's Tasks are terminal, or ownerReference them to a per-run ConfigMap)
so the base x builds Tool set does not grow unbounded.

### H5 - Observability + ops runbook
Metrics/logs (tasks created/succeeded/failed/ingested, mean iters, tokens), alert
on failure rate + ingestion lag, and a runbook under experimental/orka.

Sequencing: H1 -> H2 -> (H3, H4 parallel) -> H5.

## Future / strategic steps

### F1 - Point the Provider at the in-cluster Dynamo/Kimi stack (drop Copilot)
Swap the Copilot Provider (11) for one aimed at the in-cluster model service and
delete the de-streaming proxy (50) + Copilot-Integration-Id header. Re-validate
quality on the in-cluster model (Kimi needed softer prompts + a higher iter cap).
Removes the only external dependency and the rate limit; the biggest unknown is
quality/tuning parity. Independent of the H-series; high value.

### F2 - Generalize the producer for a second consumer
Only capz today. Generalize per-build tool routing + skills for another consumer
(capi/kubelet/dynamo/qwen) and validate one more end to end.

### F3 - Larger adjudicated batch / outcome ground truth
Beyond the 15-failure A2: a larger adjudicated batch + outcome-based labels with a
CAPZ expert to firm up the comparable-or-better claim.

### F4 - Port remediation (issues / fix-PR) to Orka
Move the on-demand issue/fix-PR agentic loop to an Orka RepositoryScan-style flow.
Unlocks the Tier C deletions (internal/actions, internal/fixpr, internal/issues,
toolloop.go, tools/).

### F5 - Decommission the in-engine analysis path (Tier A deletions)
Once Orka is the default and F1-F4 are proven, delete the fetcher's in-process
agentic analysis (service.go, agentic.go, critique.go, cache.go, modules/,
pattern/semantic) and add a Helm `analysis: orka|inprocess` switch.

### F6 - Productization decision
Fork vs keep-on-branch vs upstream-as-selectable-backend, based on manager
interest + F1-F5 outcomes.
