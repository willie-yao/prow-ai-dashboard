# Orka migration plan (on the `orka` branch)

Goal: produce a real dashboard for one consumer (capz) end to end through Orka,
on the `orka` branch, without rewriting the parts that already work.

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
- (D, lead) Build param: add an optional `build` field to each GCS tool schema;
  the producer seeds the build prefix in the Task systemPrompt and instructs the
  model to pass it on every tool call; the shim caches a Browser per build and
  falls back to BUILD_PREFIX when absent. Simplest; relies on the model passing
  the param (Copilot+Claude followed such instructions reliably in the spike).
- (L, robust fallback) Per-build tool routing: shim serves
  `/b/<build>/tool/<name>`; the producer creates per-build Tool CRDs whose URLs
  embed the build and the Task references those names. Model-independent, at the
  cost of Tool CRD churn (~tools x active-builds).

Prototype D first; keep L in reserve. Exit: two Tasks on different builds run
concurrently against one shim and each reads its own build.

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

A CronJob (or Make target for the demo) that runs fetcher(no-AI) -> producer ->
poll until Tasks settle -> ingestor -> publish the data dir the frontend serves.
Manifests under `experimental/orka/`. gcs-tool + copilot-proxy + Orka already
deployed. Exit: a real capz dashboard rendered from Orka-produced analyses.

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
