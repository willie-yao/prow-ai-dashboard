# Onboarding a new project

This is the worked example for adding a new prow-ai-dashboard project, in
either a dedicated repo or a subdirectory of an existing one.
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

A dashboard is six small files. They can live in a **dedicated repo** (e.g.
`<org>/<project>-prow-ai-dashboard`) or in an **existing repo** that doesn't
already publish a GitHub Pages site (a project repo, a tools repo, anywhere
the maintainers want PRs reviewed). Everything else is reused from this
engine repo at deploy time.

```
<your-host-repo>/
├── <project_dir>/                     # repo root, or a subdir of your choice
│   ├── project.yaml                   # bucket, branding, AI endpoint
│   └── prompts/system.md              # mandatory AI prompt addendum
├── README.md                          # optional, useful in a dedicated repo
├── LICENSE                            # Apache 2.0 recommended
└── .github/workflows/
    ├── deploy.yml                     # ~20 lines, calls reusable workflow
    └── clear-cache.yml                # ~10 lines, calls reusable workflow
```

The `project_dir:` input on `reusable-deploy.yml` points at wherever
`project.yaml` lives. For a dedicated dashboard repo, the root (`.`) is
cleanest; for an existing repo, a subdirectory like `.prow-dashboard/`
or `dashboard/` keeps the configs out of the way of the rest of the
codebase.

No Go code, no React code, no engine fork. If you find yourself adding
any of those, file an issue against the engine instead.

## Job type coverage

The engine ingests both **periodic** and **presubmit** jobs. Periodics are
enabled by default; opt into presubmits per project via either
`source.include_presubmits: true` in `project.yaml` or `include-presubmits:
true` on the reusable workflow (either toggle enables them).

Presubmit builds live under `pr-logs/pull/<org>_<repo>/<pr#>/<job>/...`;
the engine generalized its bucket URL helpers in Phase E to route between
periodic and presubmit GCS paths automatically.

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
   `source.test_infra_paths` (list of ≥1 directory), `testgrid.dashboard`,
   `gcs.bucket`, `branding.*`, `artifacts.collector`, `ai.module`. Set
   `source.file_prefix` when all your job files share a prefix; omit it
   for dashboards that span multiple files without one. Skip the
   categories block (the engine will use a sensible default).
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

### `source.test_infra_paths`

A list of one or more directories under the kubernetes/test-infra repo
root. The engine fetches every `*.yaml` under each (no recursion) and
keeps jobs that advertise the configured `testgrid.dashboard`. Most
single-SIG projects use a single path (e.g. CAPI:
`config/jobs/kubernetes-sigs/cluster-api`); cross-SIG dashboards like
`sig-node-kubelet` list multiple (`config/jobs/kubernetes/sig-node`,
`config/jobs/kubernetes/sig-cluster-lifecycle`, ...).

### `source.file_prefix`

Optional. When set, the engine keeps only files whose name starts with
this prefix. CAPI uses `cluster-api-`, CAPZ uses
`cluster-api-provider-azure-`. Omit (or leave empty) for dashboards
whose jobs span multiple files without a shared prefix; the
`testgrid-dashboards` annotation is then the sole filter and every
`*.yaml` in each path is parsed.

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

## Step 4: pick a host repo

You have two options. The engine doesn't care which you pick — both end
up at `https://<org>.github.io/<repo>/`.

**Option A — dedicated dashboard repo** (used by CAPI + CAPZ today):

```
gh repo create your-org/your-prow-ai-dashboard --public \
  --description "AI-powered dashboard for <your project> E2E tests"

git clone https://github.com/your-org/your-prow-ai-dashboard
cd your-prow-ai-dashboard
# Copy the six files into the root; use project_dir: . in the workflow.
git add -A && git commit -m "Bootstrap prow-ai-dashboard"
git push -u origin main
```

Best when the dashboard is a standalone effort or you want a separate
PR review surface for dashboard config changes.

**Option B — existing repo** (e.g. the project's own source repo, a
tools repo, or a sandbox):

Add the configs to any subdirectory that doesn't conflict with the
existing layout, plus the two workflows under `.github/workflows/`:

```
cd path/to/your-existing-repo
mkdir -p .prow-dashboard/prompts .github/workflows
cp /tmp/project.yaml .prow-dashboard/project.yaml
cp /tmp/system.md   .prow-dashboard/prompts/system.md
cp /tmp/deploy.yml .github/workflows/dashboard-deploy.yml
cp /tmp/clear-cache.yml .github/workflows/dashboard-clear-cache.yml
# In each workflow, set: project_dir: .prow-dashboard
git add .prow-dashboard .github/workflows && git commit -m "Add prow-ai-dashboard"
git push
```

Best when you want dashboard config PRs reviewed by the project's
existing maintainers, or want to avoid creating another repo.

**Caveat — GitHub Pages capacity.** A repo can only have one Pages
source. If your existing repo already publishes to Pages (a project
website, an mdBook, etc.), enabling the dashboard's `actions/deploy-pages`
flow will replace it. Use Option A in that case, or skip ahead to
[Optional: AI endpoint unreachable from GitHub-hosted runners](#optional-ai-endpoint-unreachable-from-github-hosted-runners)
for the `skip-fetch` and self-hosted-runner patterns. Pluggable non-Pages
deploy targets (Netlify, gh-pages branch in a different repo, etc.) are
tracked as Phase J in the engine plan.

## Step 5: manual GitHub config

These two steps cannot be scripted from the engine and must be done by
the host repo owner. Replace `your-org/your-repo` with whichever repo
you picked in Step 4.

```
# Enable GitHub Pages with the workflow source
gh api repos/your-org/your-repo/pages -X POST -F build_type=workflow

# Set the AI token secret (the gh CLI will prompt for the value)
gh secret set AI_TOKEN --repo your-org/your-repo
# Optional notification secret
gh secret set SLACK_WEBHOOK_URL --repo your-org/your-repo
```

## Step 6: first deploy + validation

```
gh workflow run deploy.yml --repo your-org/your-repo
gh run watch --repo your-org/your-repo --exit-status
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

## Optional: AI endpoint unreachable from GitHub-hosted runners

If your AI endpoint lives behind a private network (Azure Private
Endpoint, K8s ClusterIP service, on-prem inference, etc.) the
GitHub-hosted runner cannot reach it. Two supported escape hatches:

**Run the fetcher locally and publish pre-fetched data.** Useful for
testing or low-frequency manual refreshes. Operator runs the fetcher
on a machine with network access (VPN, `kubectl port-forward`, etc.),
commits the `data/` directory, then triggers a deploy with
`skip-fetch: true`:

```bash
cd <engine-checkout>/backend
go build -o /tmp/fetcher ./cmd/fetcher/
AI_ENDPOINT="http://localhost:8000/v1/chat/completions" \
AI_TOKEN="<key or empty>" \
AI_MODEL="<model id>" \
/tmp/fetcher -project-dir=<your-consumer-repo> \
  -out=<your-consumer-repo>/data -ai

cd <your-consumer-repo>
git add data/ && git commit -m "Refresh prefetched data" && git push
gh workflow run deploy.yml -f skip-fetch=true   # consumer workflow forwards this
```

The deploy workflow's `skip-fetch: true` input bypasses the fetcher
and publishes `<project_dir>/data/` directly. Cache restore/save
steps are also skipped.

**Use a self-hosted runner with cluster-internal access.** For
sustained automated runs, deploy `actions-runner-controller` (ARC)
in the cluster that hosts your endpoint, then forward
`runs-on:` through the consumer workflow:

```yaml
uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
with:
  project_dir: .
  runs-on: '["self-hosted", "arc-your-runner"]'   # JSON array for multi-label
  ai-endpoint: http://your-svc.ns.svc.cluster.local:8000/v1/chat/completions
```

`runs-on:` accepts a bare label (e.g. `ubuntu-latest`) or a
JSON-encoded array. No engine changes needed beyond these workflow
inputs.

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
