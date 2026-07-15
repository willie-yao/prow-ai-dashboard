# Orka backend: architecture, evaluation, and recommendation

How the Orka analysis path works, what the evaluation found, and the
productization call. For how to run it, see [USAGE.md](USAGE.md).

## Architecture: reuse the fetcher, move only the AI step

The engine's flow is: discover jobs/builds/failing tests -> analyze each failure
with the in-process agentic loop -> write dashboard.json + jobs/*.json -> serve.
Only the ANALYSIS step moves to Orka; discovery and output are unchanged.

```
fetcher -ai=false  ->  dashboard.json + jobs/*.json   (skeleton, no AI)
orka-producer      ->  one content-addressed Orka Task per failing test
                       + per-build, header-routed Tool CRDs
Orka ai-worker     ->  runs the agentic loop per Task, calling the shim tools
orka-ingestor      ->  patches each Task's result into jobs/*.json
server             ->  unchanged
```

Key design points:

- **Content-addressed, run-once.** Task name = `az-<build>-<failureHash>-<version>`.
  Prow build IDs are globally unique, so re-applying an existing Task is a no-op
  (the K8s object store is the cache). Bump `-version` to force re-analysis.
- **Per-build, per-bucket tool routing.** A single GCS tool shim serves every build
  and bucket. The producer clones the base Tool CRDs per distinct build with static
  `X-Build-Prefix` and `X-Bucket` headers, so a tool always reads the Task's own
  build/bucket regardless of what the model does. (A model-passed `build` param was
  prototyped and rejected: unreliable, and silent fallback produced wrong-build
  analyses.) One shim backs many concurrent Tasks across consumers.
- **Prompt reuse.** The producer composes the SAME system prompt as the engine
  (BasePrompt + the consumer's `prompts/system.md` + response-format footer) plus a
  tool-usage/self-critique addendum.
- **Worker patches.** Orka's ai-worker needs four changes for reliable operation on
  smaller models (iteration cap, forced finalization, empty-final re-prompt,
  transient-critique gate). See [worker-patches/](worker-patches/).

## What was proven (capabilities)

- End-to-end analysis, Kubernetes-native: multi-build + multi-bucket routing,
  event-driven webhook ingestion, Task retries, per-build Tool GC, and a coverage
  `/status` surface.
- Multi-consumer: validated on CAPZ (bucket `kubernetes-ci-logs`, cluster-per-test)
  and Istio (bucket `istio-prow`, Go integration tests, no CAPI/Azure) with the same
  code; the producer derives its tool set + bucket from each consumer's project.yaml.
- Providers: any OpenAI-compatible endpoint works with just a `Provider` (validated
  on in-cluster Kimi via Ray Serve, no proxy); Copilot needs the de-streaming proxy.

## Evaluation: quality vs the engine (the honest read)

15 diverse CAPZ failures were analyzed by both the engine (reference labels) and the
Orka path, then adjudicated against the raw GCS artifacts. Full write-up:
`files/orka-f3-report.md` in the evaluation workspace.

- **Grounding / determinism (Orka + claude):** 35/35 cited artifact paths exist in
  GCS (no hallucination); is_transient stable on 4/5 across 3 runs.
- **Expert adjudication:** on the 10 hardest disagreements, the Orka+claude
  classification was as good or better than the engine's reference on every one
  (native clearly correct on 6, better attribution on 2, split on 2, worse on 0).
- **THE CONFOUND (decisive):** that comparison was NOT same-model. The engine
  reference ran Copilot `gemini-3.5-flash` (the production `AI_MODEL`); the Orka side
  ran Copilot `claude-sonnet-4.5`. A same-model control (Orka on gemini-3.5-flash)
  showed:
  - The wins were MODEL-driven: on the decisive azl3 cases, holding the Orka harness
    constant and swapping claude->gemini flips the answer from right to wrong - the
    same mistake engine+gemini makes.
  - A robustness reversal: Orka+gemini answered only 6/15 (the rest dug deep but
    never emitted final JSON), whereas the engine's tuned config labels all 15. With
    the same cheap model, the Orka harness was WORSE.

So the engine's harness (convergence machinery, always-on critique, on-disk cache)
does real, load-bearing work that a cheap model needs; Orka's quality edge was the
strong model, not the harness.

## Harness parity: convergence + transient discipline

Two worker changes close the same-model gaps (measured on Orka + gemini-3.5-flash,
same 15):

- **Convergence (G-Converge):** valid final analyses 6/15 -> 9/15 (forced tools-free
  finalization near the budget) -> **15/15** (re-prompt when the model returns an
  empty final message - the decisive fix; the failing tasks were terminating early
  with empty content, not looping to the cap).
- **Transient discipline (G-Critique):** transient verdicts that consulted
  verify_timeline 3/8 -> **11/11** (re-prompt an is_transient=true that never called
  verify_timeline, per the engine's "confirm or default to bug" contract).

Honest limit: G-Critique enforces the DISCIPLINE, not the CLASSIFICATION. On gemini
the azl3 cases stayed mislabeled transient - the weak model now calls verify_timeline
but still misreads it. After both patches Orka is at PROCESS parity with the engine on
a cheap model; the residual quality gap is pure model capability, not harness.

## Remediation (feasibility)

Orka has a native remediation primitive, the `RepositoryScan` CRD (`forkRepo`,
`patchAgentRef`, `prBaseBranch`, `validationMode`) - but it is a PROACTIVE repo
scanner, a different paradigm from the engine's REACTIVE, CI-failure-driven fix-PR. A
failure-driven port would follow the proven analysis-port pattern (a repotree
source-tree tool shim + a fix producer Task + a fix ingestor reusing the engine's
`ghpr` / `issues`). It is ~3500 LOC and not built; adopt RepositoryScan as an optional
proactive-scan complement rather than a fix-PR replacement.

## Recommendation

1. **Keep both paths (superset, not rewrite).** Do not delete the in-engine analysis.
   Expose an `analysis: inprocess | orka` selection (today: the fetcher `-ai` flag
   plus which pipeline runs); choose per consumer by cost/quality need.
2. **Use Orka where it wins:** co-located with an in-cluster strong model, for
   consumers that want Kubernetes-native operation and can afford the model. Use the
   engine's in-process path where a cheap hosted model at low cost is the priority.
3. **Upstream the worker patches** (empty-final re-prompt, forced finalization,
   transient-critique); they are generally useful and remove the carry cost.
4. **Keep it on the `orka` branch as a selectable backend, not a fork.** The engine
   and Orka paths share the fetcher, tools, prompt composition, and output; forking
   duplicates maintenance.
5. **Revisit a fuller migration** only when a strong in-cluster model is affordable at
   fleet scale, or the harness gaps (cache, cheap-model classification) close.

Bottom line: adopt Orka as an optional, co-located, strong-model backend and a
proactive-scan complement; keep the engine as the default cheap-model path; do not
decommission.

## Deletion map (only if a full migration is later chosen)

- **Tier A (obsoleted by the Orka analysis path):** `internal/ai/service.go`,
  `agentic.go`, `critique.go`, `cache.go`, `modules/`, `pattern*.go`, `semantic.go`,
  and the fetcher `-ai=true` wiring; `cmd/worker`.
- **Tier B (reused by the Orka path, must stay):** `compose.go` / `baseprompt.go` /
  `responseformat.go`, the `ai.Client` + `tools/`, `skills/`, and all discovery/output
  packages.
- **Tier C (only if remediation is also ported):** `internal/actions`,
  `internal/fixpr`, `internal/issues`, `toolloop.go`.

Nothing is deleted now; this is gated on the F6 decision.
