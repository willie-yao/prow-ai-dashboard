# Onboarding a new project

This is the worked example for shipping a new prow-ai-dashboard consumer.
It uses [`willie-yao/capi-prow-ai-dashboard`][capi] (the Cluster API core
dashboard) as the reference because CAPI core hits the largest number of
edge cases: empty `cluster_name_prefix`, mixed unit + E2E + conformance
job types, and a non-cron periodic schedule field. Anything simpler than
that maps onto this guide trivially.

The first production consumer, [`willie-yao/capz-prow-ai-dashboard`][capz],
is a thinner case (single-provider VM-based E2E) and is referenced where it
diverges.

[capi]: https://github.com/willie-yao/capi-prow-ai-dashboard
[capz]: https://github.com/willie-yao/capz-prow-ai-dashboard

## What you ship

A consumer repo owns exactly six files. Everything else is reused from
this engine repo at deploy time.

```
your-prow-ai-dashboard/
├── project.yaml                       # bucket, branding, AI endpoint
├── prompts/system.md                  # mandatory AI prompt addendum
├── README.md                          # one-pager linking back to engine
├── LICENSE                            # Apache 2.0 recommended
└── .github/workflows/
    ├── deploy.yml                     # ~20 lines, calls reusable workflow
    └── clear-cache.yml                # ~10 lines, calls reusable workflow
```

No Go code, no React code, no engine fork. If you find yourself adding
either, file an issue against the engine instead.

## Job type coverage

The engine ingests **periodic jobs only** today. Two coupled defaults
enforce this:

1. `fetcher` ships with `-periodic-only=true`, which keeps only jobs that
   carry one of `minimum_interval:`, `interval:`, or `cron:` in their
   Prow YAML.
2. `internal/gcs/bucket.go` hardcodes the `logs/` prefix. Presubmit
   builds live under `pr-logs/pull/<org>_<repo>/<pr#>/<job>/<build>/`
   in the same bucket and would 404 if discovered.

Presubmit support is tracked as Phase E in the engine plan. Until then,
treat any presubmit-only job as out of scope and rely on the periodic
that exercises the same suite.

## Step 0: sweep the jobs first

Before writing `project.yaml`, prove the engine can discover your jobs
and confirm the category rules you intend to declare.

1. Check out the engine repo.
   ```
   git clone https://github.com/willie-yao/prow-ai-dashboard
   cd prow-ai-dashboard/backend
   go build -o /tmp/fetcher ./cmd/fetcher
   ```
2. Write a throwaway `project.yaml` with the minimum fields:
   `source.test_infra_path`, `source.file_prefix`, `testgrid.dashboard`,
   `gcs.bucket`, `branding.*`, `artifacts.collector`, `ai.module`. Skip
   the categories block (the engine will use a sensible default).
3. Run a sweep:
   ```
   mkdir -p /tmp/sweep && cd /tmp/sweep
   cp /path/to/throwaway/project.yaml .
   echo "# placeholder" > prompts/system.md   # mandatory; empty fails
   /tmp/fetcher -project-dir=. -ai=false -builds=1
   python3 -c "import json; d=json.load(open('data/dashboard.json')); \
     print(len(d['jobs']), 'jobs'); \
     from collections import Counter; \
     print(Counter(j.get('category','none') for j in d['jobs']))"
   ```
4. Read the job list. Adjust your category rules so the bucket
   distribution matches your team's mental model. Re-run until happy.

This is the step the CAPI onboarding caught a hidden engine bug
(`interval:` vs `minimum_interval:`): jobs use a variety of schedule
fields, and the sweep is where you discover yours.

## Step 1: `project.yaml`

Start from [`configs/example/project.yaml`](../configs/example/project.yaml).
The annotated fields document every knob; below are the ones that
trip people up.

### `source.file_prefix`

The engine globs `config/jobs/<test_infra_path>/*` then keeps files whose
name starts with `file_prefix`. CAPI uses `cluster-api-` (matches
`cluster-api-main-periodics.yaml`, `cluster-api-prowjob-gen.yaml`, etc.);
CAPZ uses `cluster-api-provider-azure-`. Pick the longest prefix that
still catches every periodic file in the directory.

### `testgrid.dashboard`

A single dashboard name. Jobs are kept only if they advertise this
dashboard in their `testgrid-dashboards` annotation. Release-branch
periodics typically advertise different dashboards (e.g. `cluster-api-core-1.13`)
and are filtered out automatically.

### `categories`

Ordered list of `{match, id, label}` triples. Rules are evaluated in
order; first lowercase substring match against the job name wins. Order
specific rules before broad ones. Example: CAPI puts `mink8s` before
`e2e` so `periodic-cluster-api-e2e-mink8s-main` lands in the mink8s
lane rather than the catch-all e2e lane.

The optional `category_display_order` lets you order sections in the UI
independently of match precedence.

### `capi.cluster_name_prefix`

Only consulted when `artifacts.collector: "capi"`. Two valid shapes:

- **Non-empty prefix.** CAPZ-style projects whose E2E suite names every
  workload cluster with a shared prefix (`capz-e2e-...`). The CAPI
  collector matches test cases to clusters by looking for that prefix.
- **Empty string.** CAPI core, where each ginkgo spec produces a workload
  cluster named after the spec (`quick-start-bxqxxs`, `md-rollout-yhse4f`).
  The collector falls back to a generic substring matcher that handles
  these. Declare the field explicitly with an empty value rather than
  omitting it, so the intent is visible.

### `ai.module`

One of `generic` or `capi`. The CAPI module pulls per-cluster context
into each AI request (controller logs, machine metadata, bootstrap
resource YAMLs); the generic module just passes the JUnit failure
message + build-log tail. Use `capi` whenever your project uses the
CAPI collector.

### `ai.evidence` (optional, `ai.module: capi` only)

Controls which extra artifacts the CAPI module fetches alongside the
build log and stack trace before each AI call. Every field is optional
and falls back to an engine default when omitted.

**Scope.** The entire `evidence:` block is interpreted only when
`ai.module: capi`, because the field meanings reference paths that are
specific to the Cluster API artifact layout
(`artifacts/clusters/<name>/machines/<vm>/` for `machine_logs` and
`artifacts/clusters/bootstrap/logs/<ns>/<deployment>/<pod>/` for
`controller_logs`). The `generic` module ignores the block and logs a
warning at fetcher startup if it is non-empty, so a misconfiguration
("I switched to generic and forgot evidence does nothing now") surfaces
loudly. A non-CAPI project that wants its own per-failure evidence
shape should add a new AI module rather than reuse these fields with a
different meaning.

The schema is a single `evidence:` block under `ai:` with three
independent lists:

- **`machine_logs`** — filenames under
  `artifacts/clusters/<name>/machines/<vm>/` to fetch the tail of.
  Engine default: `kubelet.log`, `containerd.log`, `journal.log`.
  Add what your project actually publishes (CAPZ extends with
  `boot.log` + `cloud-init-output.log`; CAPI core adds `kern.log`).

- **`controller_logs`** — namespaces under
  `artifacts/clusters/bootstrap/logs/` to walk for controller
  `manager.log` files. The collector picks one log per deployment in
  each namespace, so a single namespace hosting multiple controllers
  (e.g. CAPZ's `capz-system` runs ASO and the CAPZ controller side by
  side) yields multiple sections in the prompt. Each entry can be a
  bare namespace string or `{namespace, pod_name_regex, container_log}`.
  Engine default: `capi-system`, `capi-kubeadm-bootstrap-system`,
  `capi-kubeadm-control-plane-system`.

- **`build_log_patterns`** — regex patterns matched against each line
  of `build-log.txt`. Matches plus 2 lines of context are inserted as
  `=== Build Log Errors ===` in the prompt. The raw tail of the build
  log is included separately, so generic `"error"` patterns are
  redundant; reserve this list for high-signal, provider-specific
  failures (CAPZ uses Azure error codes like `SkuNotAvailable`).
  Engine default: `FAIL|\[FAIL\]`, `timed?\s*out|timeout`,
  `ImagePullBackOff|ErrImagePull`, `CrashLoopBackOff`,
  `NotFound|not found`.

Of the three, only `build_log_patterns` is conceptually module-agnostic
(every prow job has a `build-log.txt`). It lives next to the
CAPI-specific fields today; if a second non-CAPI module ever wants
build-log filtering, this field will be split out.

Override semantics are nil-vs-empty:

- Omit a field → engine default applies.
- Set to `[]` → the engine default is suppressed and no values are
  collected for that source.
- Provide a list → that list replaces the engine default in full
  (lists are not merged).

Regex patterns are validated at fetcher startup, so a typo fails fast
instead of silently dropping evidence on the next failure.

Sources the consumer asks for that aren't present in the build's
artifacts surface as a `Configured but missing` footer in the prompt,
so the model knows not to infer absence as signal.

### `ai.agentic` (optional)

An alternative to the curator-driven evidence pipeline. Instead of
the engine pre-fetching the artifacts listed under `ai.evidence`, the
model browses the build's GCS artifact tree itself via four function-
calling tools (`list_artifacts`, `read_artifact`, `tail_artifact`,
`grep_artifact`). Useful for projects whose artifacts don't fit the
CAPI layout, or for failures that happen before any cluster is
created.

Opt-in and off by default. See [docs/agentic.md](agentic.md) for the
full reference (config schema, endpoint requirements, cost notes,
fallback semantics). The `enabled: true` / `always: false` shape is
the common case: the engine's `capi` module then routes only the
failures with missing or empty `ClusterArtifacts` through agentic
mode and leaves the rest on the curator path.

## Step 2: `prompts/system.md`

Mandatory. The fetcher hard-errors at startup if the file is missing or
whitespace-only and `-ai` is enabled. There is no default project
prompt; see [docs/writing-prompts.md](writing-prompts.md) for what
sections to include, and use the [CAPI core][capi-prompt] or
[CAPZ][capz-prompt] system prompt as a starting template.

[capi-prompt]: https://github.com/willie-yao/capi-prow-ai-dashboard/blob/main/prompts/system.md
[capz-prompt]: https://github.com/willie-yao/capz-prow-ai-dashboard/blob/main/prompts/system.md

## Step 3: workflows

Both workflows are thin callers of the engine's reusable workflows.
Copy from CAPI's [`.github/workflows/deploy.yml`][capi-deploy] and
[`.github/workflows/clear-cache.yml`][capi-clear]. The only field worth
adjusting is the deploy cron (CAPI + CAPZ both use `*/30 * * * *`).

[capi-deploy]: https://github.com/willie-yao/capi-prow-ai-dashboard/blob/main/.github/workflows/deploy.yml
[capi-clear]: https://github.com/willie-yao/capi-prow-ai-dashboard/blob/main/.github/workflows/clear-cache.yml

## Step 4: repo bootstrap

```
gh repo create your-org/your-prow-ai-dashboard --public \
  --description "AI-powered dashboard for <your project> E2E tests"

git clone https://github.com/your-org/your-prow-ai-dashboard
cd your-prow-ai-dashboard
# copy the six files
git add -A && git commit -m "Bootstrap prow-ai-dashboard consumer"
git push -u origin main
```

## Step 5: manual GitHub config

These two steps cannot be scripted from the engine and must be done by
the repo owner.

```
# Enable GitHub Pages with the workflow source
gh api repos/your-org/your-prow-ai-dashboard/pages -X POST -F build_type=workflow

# Set the AI token secret (the gh CLI will prompt for the value)
gh secret set AI_TOKEN --repo your-org/your-prow-ai-dashboard
# Optional notification secret
gh secret set SLACK_WEBHOOK_URL --repo your-org/your-prow-ai-dashboard
```

## Step 6: first deploy + validation

```
gh workflow run deploy.yml --repo your-org/your-prow-ai-dashboard
gh run watch --repo your-org/your-prow-ai-dashboard --exit-status
```

After the run goes green, check the deployed site:

- `https://<org>.github.io/<repo>/` returns 200.
- `https://<org>.github.io/<repo>/data/manifest.json` reflects your
  branding (title, source repo, dashboard).
- `https://<org>.github.io/<repo>/data/dashboard.json` lists your jobs
  with categories populated; the count should match Step 0's sweep.
- For at least one failing job, fetch
  `https://<org>.github.io/<repo>/data/jobs/<job-name>.json` and
  confirm that failed `test_cases` carry an `ai_summary` whose content
  references real symbols from your project (controllers, custom
  resources, spec names) rather than generic phrasing.

If the AI summaries read generically — "the build failed during a test"
without naming any of your CRs or controllers — your `prompts/system.md`
needs more specifics. Iterate on the prompt, clear the AI cache via
the `Clear AI Cache` workflow, and redeploy. Two or three iteration
cycles is normal.

## Worked-example artifacts

The complete file set produced by following this guide is visible in
the CAPI consumer repo:

| File | CAPI core |
| --- | --- |
| `project.yaml` | [link][capi-project] |
| `prompts/system.md` | [link][capi-prompt] |
| `.github/workflows/deploy.yml` | [link][capi-deploy] |
| `.github/workflows/clear-cache.yml` | [link][capi-clear] |

CAPZ is also instructive as a contrast (`cluster_name_prefix` set,
single job family):

| File | CAPZ |
| --- | --- |
| `project.yaml` | [link][capz-project] |
| `prompts/system.md` | [link][capz-prompt] |

[capi-project]: https://github.com/willie-yao/capi-prow-ai-dashboard/blob/main/project.yaml
[capz-project]: https://github.com/willie-yao/capz-prow-ai-dashboard/blob/main/project.yaml
