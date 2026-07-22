# Orka container analyzer spike assessment

Date: July 22, 2026

## Decision

Keep the dashboard-owned container execution model as an experimental option, but
do not integrate it into the supported Orka analysis path yet.

The spike proves that Orka can own Task, Job, retry, placement, and durable result
lifecycle while the existing dashboard `FailureAnalyzer` remains the only model
policy implementation. The remaining gaps are result framing, project bundle
delivery, and durable private traces. The smallest next step is an Orka change
that gives direct container Tasks a supported stdout-only result marker or an
authenticated result channel without restoring the mounted ServiceAccount token.
If that cannot be added, an AgentRuntime prototype is justified.

## Prototype shape

```text
FailureAnalysisRequest
  -> content-addressed type: container Task
  -> dashboard analyzer image
  -> shared analysisruntime wiring
  -> ai.Service.AnalyzeFailure
  -> final FailureAnalysisResult JSON line
  -> Orka pod-log result collection
  -> strict final-line extraction
  -> existing TestCase.AISummary and TestCase.AIAnalysis
```

The Task has no `spec.ai`, ProviderRef, model field, Tool list, AgentRuntime, or
compatibility worker reference. The dashboard process runs the existing AI
Client, storage Browser, in-process Tool registry, merged skills, critique gate,
semantic judge, cache implementation, and private trace implementation.

## Adapter size

Production code added by the spike is 729 lines:

| Area | Lines |
| --- | ---: |
| Analyzer command | 158 |
| Shared runtime wiring | 219 |
| Bounded request transport | 98 |
| Orka container Task and result adapter | 254 |

The Orka-specific adapter is 254 lines. The shared runtime extraction replaces
about 150 lines of fetcher-local setup rather than copying the agentic loop or
policy.

## Runtime setup

Runtime setup was extracted into `backend/internal/analysisruntime` and the
fetcher now uses it. The extraction is behavior-preserving:

- provider resolution and prompt composition are unchanged
- engine and consumer skills still load through `skills.LoadForTools`
- filesystem and Kubernetes Tools still use the existing registry
- context-window budget detection and fixed GCS safety budget are unchanged
- critique and semantic judge settings are unchanged
- cache keys and cache schema are unchanged
- source-repository grounding remains available to the fetcher pattern pass

No agentic policy, critique rule, diagnostic skill, evidence renderer, cache
schema, or Tool implementation was copied or changed.

## Request transport

The prototype uses one JSON environment value:

- environment name: `PROW_AI_FAILURE_REQUEST`
- hard limit: 64 KiB
- strict JSON decoding with unknown-field and trailing-value rejection
- required job, build, build prefix, failed test name, and status validation
- canonical SHA-256 in `PROW_AI_FAILURE_REQUEST_SHA256`
- request digest, analyzer image, command, args, timeout, retries, Secret
  references, and contract version participate in Task identity

Model and storage credentials are not part of the request. The Task builder uses
Kubernetes `secretKeyRef` values for credentials.

A production transport should use an immutable object-storage reference once
requests approach the inline limit or need independent retention. The object
must be content-addressed, scoped to one project, and readable without putting a
credential in the Task request.

## Result transport

The reliable prototype path is container log collection.

The analyzer writes one compact `FailureAnalysisResult` JSON object as its final
stdout line and sends all other logs to stderr. The pinned Orka controller does
not preserve the stream distinction when it reads Kubernetes pod logs, so the
stored Task result contains both streams. Experimental ingestion therefore
strictly parses the final non-empty line as `FailureAnalysisResult` and rejects
missing, malformed, oversized, or structurally empty results.

Direct result POST was not used. The pinned Orka job builder sets
`automountServiceAccountToken: false` for a direct custom container image, while
the result endpoint requires authenticated caller identity. Injecting a token
manually would bypass the direct-container security decision, so the spike did
not do that.

This is good enough to prove lifecycle behavior, but mixed-stream framing is not
a production contract. An Orka stdout marker for container Tasks would remove
the final-line convention without weakening token policy.

## Honest failure behavior

- malformed or oversized requests fail before runtime initialization
- missing or mismatched request digest fails the process
- `AnalyzeFailure` errors produce no stdout result and a nonzero process exit
- cache or trace persistence errors also fail before result publication
- Orka retries the failed process according to `retryPolicy`
- generated Kubernetes Jobs keep `backoffLimit: 0`
- malformed or absent result JSON fails experimental ingestion
- the ingestor mapping assigns the returned `AISummary` and `AIAnalysis` without
  a second policy or acceptance gate

## Telemetry

Retained in Orka:

- Task pending, running, succeeded, and failed lifecycle
- Kubernetes Job and Pod lifecycle
- Task attempt count and controller retry
- CPU placement and timeout state
- durable Task result availability

Retained by the dashboard runtime:

- sanitized model request metadata
- Tool call names, outcomes, bytes, and timing
- cache, critique, and semantic judge state
- the existing `AIAnalysis` counters

Not retained as first-class Orka events:

- ModelRequest events
- ToolCall events
- critique and semantic judge events

The analyzer writes the normal private trace file under its data directory, but
the direct container Task has no persistent dashboard volume. The current kind
prototype therefore retains readable sanitized runtime logs in the Orka result,
while the full private trace file is ephemeral. This is sufficient for a spike,
not for production operators. A supported trace sink or project data volume is
required before integration.

## Local kind result

`experimental/orka/run-container-analyzer-kind.sh` created and deleted an
isolated three-node kind cluster using pinned Orka commit
`1b6f6f74c8cdf5e3ccfe92d0a7ed03a571670254`.

The scripted Flatcar case passed end to end:

- Orka Task phase: `Succeeded`
- Task result parsed as the dashboard `FailureAnalysisResult`
- Tool calls: 3
- artifact bytes: 48,703
- critique passed
- semantic judge passed
- benchmark score: 5 of 5 signals

The retry case intentionally failed the first analyzer process at the model
endpoint and then succeeded with `status.attempts >= 2`. The Job used
`backoffLimit: 0`, so retry ownership remained in Orka.

The cluster had `agentpool=nodepool1` and a mock `agentpool=h100` node. Analyzer
Tasks, the scripted model, and the Orka controller were constrained to the CPU
pool. The unused AgentRuntime harness wrapper was scaled to zero. The smoke test
asserted that no remaining Orka namespace pod scheduled on the mock GPU node.
The test removed its labeled Tasks, Jobs, Pods, Deployment, Service, ConfigMap,
and Secret. The wrapper script deleted the kind cluster and temporary image
context.

No H100, RayService, Ray pod, live dashboard deployment, or live Kimi endpoint
was touched.

## Operational dependencies

A production container path still needs:

- an immutable dashboard analyzer image
- a supported way to provide the consumer `project.yaml`, prompt, and skills
- Secret references for the model and any private storage credentials
- a CPU node label contract such as `agentpool=nodepool1`
- persistent Orka result storage
- a persistent dashboard cache and trace location, or external sinks
- a first-class result framing contract for direct containers

The kind script bakes the generated project bundle and Flatcar fixture into a
throwaway image. That is appropriate for the benchmark, not a proposed consumer
packaging model.

## AgentRuntime comparison

| Topic | Container Task | AgentRuntime |
| --- | --- | --- |
| Adapter size | Small direct Task and final result parser | Requires a harness service and turn protocol |
| Execution | One process in one Orka Job | Orka calls a long-running runtime endpoint |
| Result | Controller pod-log fallback today | Structured harness response |
| Events | Task and Job lifecycle only | Harness turn lifecycle can be preserved |
| Dashboard policy | Runs directly in the analyzer binary | Still runs in a dashboard-owned harness |
| Extra protocol | None beyond Task and request JSON | Capabilities, turn, auth, cancellation, and error mapping |
| Project bundle | Still needs delivery | Still needs delivery unless built into the harness |

AgentRuntime would give a cleaner structured result channel and could preserve a
richer turn lifecycle. It also adds a network service, authentication, protocol
versioning, cancellation, and harness compatibility surface for a one-shot
analysis that already maps naturally to a Job.

A second prototype is worth doing only if the direct container result and trace
gaps cannot be solved in Orka with a small upstream contract. Container Tasks
remain the simpler execution model for this workload.
