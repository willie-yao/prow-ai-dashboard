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

### H1 - Containerize producer + ingestor (DONE)
The `experimental/orka/Dockerfile` builds any orka binary via `--build-arg CMD=`;
`orka-producer` and `orka-ingestor` build as ~15-18MB distroless images. The
fetcher stays in the main engine image. No fetcher duplication.

### H2 - In-cluster CronJob (RBAC, client-go apply) (DONE)
Ported `run-demo.sh` into an in-cluster CronJob against the shared PVC:
fetcher(-ai=false) initContainer -> orka-producer `-apply` initContainer ->
orka-ingestor `-wait` container. Client-go (DECIDED above) lives only in
`orkamig` (`kube.go`: `RESTConfig`, `KubeClient.Apply` via server-side apply,
`TaskPhase`), so the engine binaries link zero client-go packages. The producer
gained `-apply`/`-context`; the ingestor gained `-wait`/`-poll` (retries the
result API until every failing test is patched) and defaults its bearer to the
mounted SA token, so the ingestor needs no k8s RBAC. Manifests:
`60-pipeline-rbac.yaml` (SA + Role/RoleBinding on tasks+tools) and
`70-pipeline-job.yaml` (the CronJob). The apply seam was validated live: the
producer server-side applied 64 Tools + 4 Tasks (field manager `orka-producer`).

### H3 - Event-driven ingestion via Task.webhookURL (DONE)
The producer's `-webhook-url` sets `spec.webhookURL` on each Task; a long-lived
`orka-ingestor -serve` Deployment + ClusterIP Service receives Orka's completion
webhook (`{taskName, phase, resultRef.available}`), fetches the result, and
patches the one matching test, or marks it unavailable on Failed/Cancelled.
Patches are mutex-serialized. Orka's SSRF guard requires a same-namespace
ClusterIP Service with a selector (satisfied by `71-ingestor-webhook.yaml`), and
delivery failures don't fail the Task (Orka retries), so the receiver can roll
safely. It composes with the CronJob rather than replacing it: the job's fetcher
still writes the skeleton and its `-wait` ingest re-applies every result from the
result store after a refresh (the backstop), while the receiver keeps results
current with low latency between runs. The receiver's `-version` must match the
producer's. Validated locally: a Succeeded webhook patched the real analysis and a
Failed webhook marked the test unavailable.

### H4 - Task lifecycle: retries, failure surfacing, Tool GC (DONE)
The producer stamps each Task with `retryPolicy.maxRetries` (`-retries`, default
1) and an `orka.dashboard/build` label on both Tasks and per-build Tools. The
ingestor, on its final pass, marks any still-unresolved failing test with the
engine's `AI analysis unavailable:` placeholder and a Task-phase reason (Task
failed / cancelled / not found) instead of leaving it silently blank. With `-gc`
it deletes a build's per-build Tools once that build's Tasks are all terminal
(unlabeled Tools are never touched). Validated live: failed/absent Tasks surfaced
as unavailable with the right reason, and GC deleted 64 build-labeled Tools after
their Tasks were gone.

### H5 - Observability + ops runbook (DONE)
The ingestor derives an analysis-coverage summary from the skeleton
(`failing`/`analyzed`/`unavailable`/`pending`), logs it as a `📊` line at the end
of every batch run, and the `-serve` receiver exposes it as JSON at `/status`
(plus `/healthz`). No new dependency (no Prometheus). The operator runbook lives
in `experimental/orka/README.md` (Pipeline runbook: deploy, operate - trigger a
run, read coverage, force re-analysis - and troubleshoot). Deliberately lean for
an experiment; a Prometheus `/metrics` surface is a later add if this graduates.

Sequencing: H1 -> H2 -> (H3, H4 parallel) -> H5. All DONE.

## Future / strategic steps

### F1 - Point the Provider at the in-cluster Kimi (Ray Serve) stack (VALIDATED)
Added `12-kimi-provider.yaml`: an `openai`-type Provider aimed at Kimi-K2 served
over an OpenAI-compatible API by Ray Serve, with NO de-streaming proxy. Unlike
Copilot's Claude, Kimi returns `tool_calls` on the non-streaming
`/v1/chat/completions` path, so the proxy (50) and the `Copilot-Integration-Id`
header are unnecessary. Validated end to end on the spike:
- hello-world Task returned `ORKA_KIMI_OK`;
- a real capz failure Task ran the full tool set, correctly routed to its own
  build (option L), and produced a grounded, artifact-cited analysis matching
  Copilot's earlier conclusion on the same failure (3rd control-plane
  `etcd-join: context deadline exceeded`, classified transient).

Topology note: the spike runs Orka on a separate kind cluster from the h100 model
cluster, so the Provider baseURL bridges to a `port-forward` via
`host.docker.internal` (verified reachable from kind pods). In the production
topology Orka co-locates with the inference stack and baseURL is the in-cluster
Service DNS (`http://kimi-0905-serve-svc.default.svc:8000/v1`).

Caveat (see F1a): Kimi's 32k context is much smaller than Copilot's, but Orka's
worker already compacts the conversation on a context-too-long error, so overflow
is not the failure mode. The real limit is the iteration budget: Kimi investigates
less efficiently than Claude and exhausted the 50-iteration cap without concluding.
Raising the ai-worker cap to 80 let the same task converge on the first attempt in
22 tool calls. The cap is the load-bearing lever (a producer-prompt tool-budget
nudge helps but is not sufficient; the H4 retryPolicy is the backstop). F1a
documents this in the README constraints.

### F1 (original) - Point the Provider at the in-cluster Dynamo/Kimi stack (drop Copilot)
Swap the Copilot Provider (11) for one aimed at the in-cluster model service and
delete the de-streaming proxy (50) + Copilot-Integration-Id header. Re-validate
quality on the in-cluster model (Kimi needed softer prompts + a higher iter cap).
Removes the only external dependency and the rate limit; the biggest unknown is
quality/tuning parity. Independent of the H-series; high value.

### F2 - Generalize the producer for a second consumer (VALIDATED on Istio)
Proved the pipeline works for a structurally different consumer: Istio (bucket
`istio-prow`, Go integration tests, no CAPI/Azure cluster-per-test model). Fixed
the capz couplings in the producer and shim:
- The shim (`orka-gcs-tool-spike`) is now multi-bucket: it resolves the GCS bucket
  per request via an `X-Bucket` header (or "bucket" body field), mirroring the
  existing multi-build design. One shim backs many consumers.
- The producer derives its tool set from the consumer's `project.yaml` `ai.tools`
  (mapping the `filesystem` / `k8s` groups to Orka Tool CRD names) instead of a
  hardcoded list, gates the cluster-navigation prompt guidance on the `k8s` tools
  being enabled, de-hardcodes the "CAPZ" project label (uses the project's short
  name), stamps the consumer's bucket as `X-Bucket` on each cloned Tool, and
  derives the build prefix from the skeleton's artifact URL so presubmit
  (`pr-logs/pull/...`) builds work, not just periodic (`logs/...`).

Validated end to end: a real failing Istio presubmit
(`integ-pilot_istio` `TestTunnelingOutboundTraffic`) analyzed on Copilot produced
a grounded, Istio-specific root cause (HTTP/2 egress-gateway tunneling strips the
hostname, causing an EOF; `is_transient=false`), citing the exact `build-log.txt`
line, with zero capz/cluster/Azure contamination. The producer emitted only the
filesystem + quality tools (no `find_my_cluster`), and the multi-bucket shim served
`istio-prow` in-cluster. The Istio consumer config
(`project.yaml` with `discovery.source: bucket`, `ai.tools: [filesystem]`, and an
Istio `prompts/system.md`) is a throwaway scaffold, not committed.

### F2 (original) - Generalize the producer for a second consumer
Only capz today. Generalize per-build tool routing + skills for another consumer
(capi/kubelet/dynamo/qwen) and validate one more end to end.

### F3 - Objective quality metrics on the current pipeline (DONE)
Re-ran the same 15 A2 failures through the current pipeline (Copilot, post-M5 +
F1a) and computed ground-truth-free metrics (full write-up: session
`files/orka-f3-report.md`):
- Grounding: 35/35 cited artifact paths exist in GCS (100%; no hallucinated
  artifacts).
- Determinism: is_transient stable on 4/5 across 3 runs (one flip on an ambiguous
  case).
- M5/F1a delta: aggregate transient rate dropped from A2's 12/15 to 8/15, now
  matching the engine's own 8/15 - the "default to bug unless proven" instruction
  removed the over-transient bias in aggregate.
- Per-case engine agreement stayed 5/15 (A2 was 7/15): the reduction redistributed
  which cases are transient; all 10 disagreements are on the same hard/ambiguous
  cases as A2, and on several (f05/f06/f08) native's "bug" call is at least as
  defensible as the engine's "transient".
- Weak spot: only 3/8 is_transient=true verdicts consulted verify_timeline. Soft
  prompt discipline is not reliably followed; a deterministic gate requiring a
  verify_timeline confirmation before is_transient=true (the engine's mechanism)
  would make the classification axis production-grade.

Remaining ground-truth work (needs a CAPZ expert or outcome data): score the 10
disagreements against artifacts / did-a-fix-land outcomes.

Expert adjudication (DONE, 2026-07-14): all 10 disagreements were decided against
the raw GCS artifacts using the debug-capz-k8s skill (full write-up in
`files/orka-f3-report.md`). Result: on every one of the 10, the Orka pipeline's
classification is as good or better than the engine's reference label - native
clearly correct on 6 (f03/f04/f05/f06/f08 real bugs the engine mislabeled
transient; f15 an Azure-extension hang the engine mislabeled a bug), better
attribution on 2 (f13/f14 upstream DRA-alpha, not CAPZ), genuinely split on 2
(f09/f11, native's client-rate-limiter mechanism correct vs the engine's
unsupported scale-down guess), and never clearly worse. Key inversions: (1) the
naive engine-agreement metric was penalizing native for being RIGHT; (2) native
is not merely "over-transient" - it discriminates correctly (real CAPZ bugs ->
bug: azl3 etcd-join, NSG reconcile; Azure/upstream -> transient/not-ours). This
measures correctness, not process: the 3/8 verify_timeline discipline gap stands,
so a deterministic gate is still warranted so correctness does not depend on the
model choosing to verify.

### F3 (original) - Larger adjudicated batch / outcome ground truth
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
