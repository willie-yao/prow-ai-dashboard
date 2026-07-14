# Orka evaluation: findings and conclusion

Durable summary of the Orka-native evaluation (the full session narrative lives
in the working notes; this is the decision-relevant distillation). Everything
here is experimental and lives only on the `orka` branch.

## Question

Could Orka (a Kubernetes-native agent-orchestration platform) run the dashboard's
AI CI-failure analysis "without any custom agent code," and is the quality
comparable to the custom Go engine?

## What was proven

1. Model access: an Orka `Provider` reaches GitHub Copilot end to end via a small
   header-injecting + de-streaming proxy (`backend/cmd/orka-copilot-proxy`).
   Copilot's non-streaming `/chat/completions` returns a null `tool_calls` array
   for Claude models, so the proxy forces streaming upstream and re-aggregates.
2. GCS/Prow tools: the engine's real artifact tools (filesystem + k8s discovery)
   plus five new deterministic quality tools are exposed over HTTP
   (`backend/cmd/orka-gcs-tool-spike`) and declared as Orka `Tool` CRDs. An Orka
   `type: ai` Task drives them through a genuine multi-round investigation.
3. Depth: with the k8s discovery tier + Copilot's full context window, the agent
   independently resolves the right per-spec cluster and reads its Azure activity
   logs - the same investigative depth as the engine's agentic loop.

## Quality: comparable to the custom engine

A 15-failure batch (engine output = reference) run through the full native stack
agreed on `is_transient` 7/15. But agreement is NOT accuracy: the engine's own
labels are not ground truth. Human-style adjudication of the hardest
disagreements against the raw artifacts found the native stack was AT LEAST AS
accurate as the engine and marginally better - more tightly grounded root causes,
and correct `is_transient` calls where the engine erred (f05 real bug, f15 Azure
platform transient), with one genuine tie (f09). See the working-notes report for
the per-case adjudication.

Conclusion: the Orka + Copilot + deterministic-tools path is a credible,
comparable-quality alternative to the custom engine.

## What "no custom agent code" really means

Orchestration (the agentic loop, tool-calling, retries, isolation) is fully
Orka's job. But you still write and run: the GCS/Prow domain Tool service, the
Copilot proxy, an ingestion shim from Orka results to `dashboard.json`, and the
frontend. The engine's deterministic quality machinery (critique gate,
investigation floors, cache/invalidation) has no direct Orka equivalent; the
native design dissolves the cache layer into content-addressed run-once Tasks and
replaces the floors/critique with a reviewer agent + deterministic tools (a
reviewer + one `validate_analysis` tool measurably improved classification in a
prototype).

## Known constraints (must-fix before real adoption)

- Orka Tool controller SSRF guard marks in-cluster (private-IP) tool URLs
  `Available: false`; the skip is test-only (no flag). Works here only because
  the worker does not gate on availability. Needs an upstream flag.
- ai-loop iteration default is 10 (too low for deep CI analysis); patched to
  30-50 from source here. Raise upstream or via coordination mode.
- Copilot Claude non-streaming tool_calls gap needs the proxy (or an upstream
  switch to streaming).
- `check_transient_signatures` matches background log noise (429/DNS/context-
  deadline appear in nearly every build); pair it with a `verify_timeline`
  confirmation and keep a "default to bug unless proven transient" contract.

## Next steps

- Larger adjudicated batch with a CAPZ expert (or outcome-based ground truth:
  did a fix PR land? did the failure recur and get triaged as infra?).
- Ingestion shim (`Task.webhookURL` -> `dashboard.json`) for an end-to-end demo.
- Point the `Provider` at the in-cluster Dynamo/Kimi stack to drop the Copilot
  dependency and test at production model scale.
- Report the three upstream constraints to the Orka team.

## Layout

See `README.md` for the reproduce steps and the manifest/Dockerfile layout. The
Go shims live under `backend/cmd/orka-*` (they import the engine's internal
packages, so they must stay in the backend module).
