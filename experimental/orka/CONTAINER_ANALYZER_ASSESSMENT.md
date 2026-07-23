# Orka container analyzer assessment

Status as of July 23, 2026.

The container analyzer tests whether Orka should own lifecycle for the
dashboard's existing `FailureAnalyzer`. It does not use Orka's AI worker,
Provider, or Tool resources. The in-process analyzer remains canonical.

## Prototype evidence

The initial local kind prototype demonstrated:

- One dashboard-owned analyzer in an Orka `type: container` Task.
- The scripted Flatcar benchmark scored 5 of 5 expected diagnosis signals.
- Orka retried a failed analyzer attempt.
- Analyzer and helper workloads stayed on CPU nodes.
- The Task result remained available through Orka's result API.

This proves the basic lifecycle shape. It does not make the path production
ready.

## Production gates

| Gate | Status |
| --- | --- |
| Structured result framing | Implemented in v2 and retained by `dashboard-failure-analyzer-v3` |
| Immutable request and project bundle | Implemented by `dashboard-failure-analyzer-v3` |
| Persistent cache and private traces | Pending |
| Clean in-process and container Kimi comparison | Pending |
| Bounded multi-failure load test | Pending |

No supported Helm analysis mode should use the container path until every gate
passes.

## Result contract

Contract v2 emits exactly one dashboard-owned result marker on stdout:

```text
PROW_AI_RESULT_B64:<base64 FailureAnalysisResult JSON>
```

Normal analyzer logs and errors remain on stderr. The parser scans mixed Task
logs for the last valid marker instead of treating the final non-empty line as
JSON. Identical duplicate markers are accepted. Conflicting valid markers are
rejected. Missing markers, malformed base64, oversized payloads, malformed or
unknown JSON fields, and empty summaries are rejected.

The decoded result is limited to 2 MiB. The Orka controller can continue storing
combined pod logs without becoming the owner of the dashboard result schema.

## Bundle contract

Contract v3 creates one immutable content-addressed ConfigMap per unique request
and consumer project bundle. The bundle contains:

- `FailureAnalysisRequest`
- Sanitized `project.yaml`
- `prompts/system.md`
- Top-level `skills/*.yaml` and `skills/*.yml`
- Schema version, analyzer contract version, and a full content digest

The Task reads the bundle through `env.valueFrom.configMapKeyRef` and receives the
expected digest separately. The analyzer verifies both digests, writes the files
to a private temporary directory, loads the normal dashboard runtime, and removes
the directory after the analysis attempt.

Provider API, endpoint, model, and headers are removed from bundled
`project.yaml`. API, endpoint, and model remain ordinary Task environment values.
Credentials remain Secret references. Bundles with `ai.headers` are rejected
until a Secret-backed custom-header contract exists. The bundle is limited to 96
KiB so one ConfigMap value stays below the Linux per-environment-value limit.

## Remaining design risks

- Consumers whose request, prompt, and skills exceed 96 KiB need an immutable
  object-storage transport instead of the ConfigMap path.
- Bundle ConfigMaps contain private failure and prompt context, so namespace
  access remains part of the deployment trust boundary.
- One-shot Tasks lose local cache and trace files unless the dashboard adds an
  explicit persistence transport.
- Orka adds scheduling and image startup overhead for every failure.
- The container adapter is useful only if operators value Task retry, history,
  placement, cancellation, and durable lifecycle state enough to justify the
  extra control plane.

## Next evaluation

Add cache and trace persistence before running the Kimi parity and load tests.
If those steps require several custom services or sidecars, remove Orka analysis
and keep the in-process path.
