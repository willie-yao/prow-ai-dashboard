# Testing

The engine has three layers of tests. The first two are deterministic CI gates;
the third is an on-demand quality harness (planned).

## Unit tests

Per-package tests live alongside the code. Run them with:

```bash
make test          # cd backend && go test ./... -count=1
go vet ./...
```

These are the CI gates (see `.github/workflows/ci.yml`).

## End-to-end pipeline tests

`internal/e2e` runs the full `fetcher.Run` pipeline (discover, fetch, parse
JUnit, aggregate, AI analysis, write output) against committed fixtures with no
network, no real model, and no GCS. They are hermetic and deterministic.

```bash
make e2e           # go test ./internal/e2e/... -count=1 -v
```

The harness relies on two seams:

- **Local storage provider** (`storage.ProviderLocal`): reads artifacts from a
  directory tree mirroring the bucket layout. The fixture `project.yaml` sets
  `storage.provider: local`, `storage.base: <fixture bucket>`, and
  `discovery.source: bucket`, so discovery and the agentic artifact tools both
  read the fixtures. This provider is also usable for offline or air-gapped
  fetches against a downloaded artifact tree. It is intended for testing and
  offline use, not for publishing a public dashboard: without `storage.web_base`
  set, artifact links are emitted as root-relative paths (never the on-disk
  root). Set `web_base` to the real public bucket URL if you publish
  offline-fetched data.
- **Scripted model** (`internal/aitest.ScriptServer`): an httptest
  chat-completions server that returns queued responses in order, so a single
  failure's agentic loop is fully deterministic. Its sibling
  `aitest.ReplayServer` serves recorded responses keyed by a request
  fingerprint, for higher-fidelity record/replay.

Fixtures live under `internal/e2e/testdata/`:

```
testdata/
  bucket/logs/<job>/<build>/{started,finished}.json
  bucket/logs/<job>/<build>/build-log.txt
  bucket/logs/<job>/<build>/artifacts/junit.xml
  prompts/system.md
```

To add a scenario, extend the fixture tree and add a test in
`internal/e2e/pipeline_test.go`.

### Recording real model responses

`aitest.ReplayServer` can capture fixtures from a real endpoint once, then
replay them deterministically. Build a recording server with an upstream URL and
token, run the analysis once to populate `testdata`, then switch the test to
`NewReplayServer`. Scrub any sensitive content from recorded responses before
committing.

## Quality evaluation harness

The `eval` harness scores AI analysis quality against a **labeled dataset** of
real failures and supports A/B comparison across models, prompts, or config.
Because model output is non-deterministic (the deep model was measured ~40%
non-deterministic run-to-run on hard failures), this is a **tracked scorecard**
run on model/prompt/config changes, not a pass/fail CI gate. It is the harness
that catches quality regressions a single smoke test would miss.

### Dataset

A dataset is a directory with `cases.json` and an `artifacts/` tree (build
trees in the bucket layout the analyzer reads). Each case carries hand-verified
ground truth:

```json
{
  "cases": [
    {
      "name": "control-plane-provisioning-timeout",
      "job": "periodic-example-e2e-main",
      "build": "101",
      "build_prefix": "logs/periodic-example-e2e-main/101/",
      "test_name": "[It] ... Creates a HA cluster with 3 control plane nodes",
      "failure_message": "Timed out waiting for 3 control plane machines ...",
      "labels": {
        "is_transient": false,
        "root_cause_keywords": ["control plane", "timed out", "register"],
        "expected_files": ["build-log.txt"]
      }
    }
  ]
}
```

See `backend/eval/dataset/example/` for a working one-case example. Grow the
dataset with real, hand-labeled failures over time (the labeling is the main
cost).

### Metrics

Each case is scored objectively, then aggregated over the **available** cases
(those that produced a result; a failed analysis is excluded from classification
so a broken run can't masquerade as accurate):

- **Coverage**: fraction of cases that produced a usable analysis. A low
  coverage means the endpoint/model failed, not that the engine was accurate.
- **Transient accuracy** and **real-bug precision/recall/F1** (positive class =
  real bug): did it classify transient-vs-real correctly?
- **Grounding rate**: did it actually investigate (made a tool call and fetched
  evidence)?
- **Citation validity**: fraction of cited files that exist in the artifacts,
  which catches hallucinated citations.
- **Expected-file recall**: fraction of the labels' `expected_files` the
  analysis cited, which catches an analysis that ignores the evidence files a
  correct diagnosis should reference.
- **Keyword recall**: fraction of expected root-cause terms present.
- **Mean tool calls / GCS bytes**: depth.

No LLM-as-judge is used in the scored path; the metrics are objective so the
scorecard is reproducible given the same analyses. Each scorecard also records
the run's `meta` (dataset fingerprint, model, prompt fingerprint, floors,
critique, samples) so an A/B comparison warns when the two sides were not
produced under comparable conditions.

### Running it

The connection comes from `AI_ENDPOINT`, `AI_MODEL`, `AI_TOKEN` (same as the
fetcher). Each case runs through the real `Service.Analyze` path with a
throwaway cache, so every analysis (and every repeated sample) is a real model
call. Pass `PROJECT_DIR=<consumer dir>` to load that project's real prompt,
skills, and agentic config (floors, critique, evidence injection, tools) so the
eval mirrors production; omit it to use the built-in default prompt and the CLI
flag defaults.

```bash
export AI_ENDPOINT=... AI_MODEL=... AI_TOKEN=...
make eval DATASET=eval/dataset/example SAMPLES=3 PROJECT_DIR=../capz-prow-dashboard
```

`SAMPLES > 1` re-runs the dataset to report run-to-run variance (mean/min/max),
which matters given the non-determinism; the persisted `scorecard.json` is the
**mean** across samples (the tracked measurement), and `summary.md` adds a
variance table. Output lands in `eval/out/`.

To A/B (e.g. compare a cheaper model or a prompt change against a saved
baseline):

```bash
make eval DATASET=... SAMPLES=5            # capture baseline -> eval/out/scorecard.json
cp eval/out/scorecard.json /tmp/base.json
export AI_MODEL=<candidate>
make eval-ab DATASET=... BASELINE=/tmp/base.json
```

The A/B summary flags a mismatch if the baseline and candidate used a different
dataset/evidence, prompt, agentic config, or sample count, so the comparison
stays apples-to-apples. A differing model is not flagged, since comparing models
is the usual point of an A/B; it is shown in each scorecard's meta line instead.

The harness itself is unit-tested hermetically (`internal/eval`): the scorers
are pure functions, and the runner is exercised with the scripted model, so no
token is needed to test the harness, only to run a real evaluation.

