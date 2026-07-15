# Orka ai-worker patches (convergence)

Changes to Orka's `workers/ai/main.go` (Orka's own source, not this repo) that the
migration needs. They are kept here as a patch because they live in the upstream
Orka worker; apply against a matching Orka checkout, rebuild the ai-worker image,
and load it into the cluster.

`ai-worker-convergence.patch` contains three changes, all in the agent loop:

1. **Iteration budget (from F1a):** `maxIterations` 10/50 -> 80. Open-weight models
   investigate less efficiently than Claude and exhausted a 50 cap without
   concluding on complex CAPZ failures.

2. **Forced finalization near the budget (G-Converge):** for the last 2
   iterations, the request is sent with NO tools plus a "budget exhausted, output
   your final JSON now" message. This guarantees a no-tool-call response the loop
   treats as completion, so a model that keeps investigating still emits an answer
   instead of erroring at the cap.

3. **Empty-final re-prompt (G-Converge, the decisive one):** when the model returns
   no tool calls AND empty content (a small-model failure mode - it terminates
   without answering), the loop re-prompts once for the final JSON instead of
   accepting an empty result.

## Why (measured)

Running the same 15 A2 CAPZ failures through Orka on `gemini-3.5-flash` (the
engine's production model), counting how many produced a valid final analysis:

| Worker | Valid analyses |
|---|---|
| baseline (cap 80 only) | 6/15 (40%) |
| + forced finalization | 9/15 (60%) |
| + empty-final re-prompt | **15/15 (100%)** |

The empty-final re-prompt was decisive: the failing tasks were not looping to the
cap, they were terminating early with an empty message (~30 tool calls, then an
empty final turn -> "empty request body" on result submission). This closes the
F3 same-model convergence gap: Orka now matches the engine's robustness at getting
a small/cheap model to produce an answer. (This is convergence only; classification
quality on a small model is still model-limited and is addressed separately by the
deterministic critique gate.)

## Upstreaming

These belong upstream in Orka (the worker should not silently submit an empty
result, and a forced-finalization at the budget is generally useful). Until then,
carry this patch. A cleaner long-term form: make the empty-final re-prompt and the
finalization reserve count configurable via the coordination env.
