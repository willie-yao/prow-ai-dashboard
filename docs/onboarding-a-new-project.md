# Onboarding a new project

The complete reference for adding a prow-ai-dashboard project. For the fast
happy path, start with [quickstart.md](quickstart.md) and come back here for the
options it skips: job grouping, presubmits, host-repo choice, version pinning,
and private chat-completions endpoints.

## What you ship

A dashboard is a few small files in a **dedicated repo** or a **subdirectory of
an existing repo** that does not already publish a GitHub Pages site. Everything
else is reused from the engine at deploy time.

```
<host-repo>/
├── <project_dir>/             # repo root, or a subdir of your choice
│   ├── project.yaml           # bucket, branding, chat-completions endpoint
│   └── prompts/system.md      # AI prompt addendum (required)
└── .github/workflows/
    ├── deploy.yml             # calls the reusable deploy workflow
    └── clear-cache.yml        # calls the reusable clear-cache workflow
```

- `project_dir` on the reusable workflow points at wherever `project.yaml`
  lives: `.` for a dedicated repo, or a subdir such as `dashboard/`.
- No Go or React code, and no engine fork.
- Both **periodic** and **presubmit** jobs are supported. Periodics are on by
  default; enable presubmits with `source.include_presubmits: true` in
  `project.yaml` (or `include-presubmits: true` on the reusable workflow).

## Step 0: sweep the jobs first

Confirm the engine discovers your jobs before writing the full config.

```bash
git clone https://github.com/willie-yao/prow-ai-dashboard /tmp/engine
cd /tmp/engine/backend && go build -o /tmp/fetcher ./cmd/fetcher

mkdir -p /tmp/sweep/prompts && cd /tmp/sweep
cat > project.yaml <<'YAML'
id: myproject
name: "My Project"
testgrid:
  dashboard: "<your-testgrid-dashboard-name>"   # e.g. "sig-release-master-blocking"
storage:
  provider: gcs
  bucket: "kubernetes-ci-logs"
branding:
  title: "My Project Prow Dashboard"
  base_path: "/<repo-name>"
  site_url: "https://my-org.github.io/<repo-name>"
  source_repo:
    owner: "<org>"
    name: "<repo>"
YAML
echo "# placeholder" > prompts/system.md     # required; empty fails
export GITHUB_TOKEN=$(gh auth token)          # avoids the 60/hr anonymous limit
/tmp/fetcher -project-dir=. -ai=false -builds=1
python3 -c "import json; print(len(json.load(open('data/dashboard.json'))['jobs']), 'jobs')"
```

A non-zero count confirms `testgrid.dashboard` is correct. Zero means the name
does not match any job's `testgrid-dashboards` annotation; fix it before
continuing. If your project has only presubmit jobs, add `source:` with
`include_presubmits: true` to the sweep config (periodics are the default).

## Step 1: `project.yaml`

Start from [`configs/example/project.yaml`](../configs/example/project.yaml),
which annotates every field. The fields that matter:

- **`storage`** (required): where the project's Prow build artifacts live. The
  engine does not assume GCS.
  - `provider` (required): the storage backend. `gcs` for native Google Cloud
    Storage (kubernetes.io Prow), or `gcsweb` for any gcsweb HTTP gateway
    fronting a bucket (e.g. an S3 bucket behind `gcsweb.<project>.io`).
  - `bucket`: the bucket name, no `gs://`/`s3://` prefix, e.g. `kubernetes-ci-logs`.
  - For `gcsweb`, also set `base` (the gateway root that serves raw objects and
    HTML listings, e.g. `https://gcsweb.istio.io/s3`) and usually `prow_base`
    (the Prow deck root, e.g. `https://prow.istio.io/view/s3`).
- **`discovery`** (optional): how the fetcher finds the project's jobs.
  - `source: testgrid` (default): kubernetes/test-infra job YAMLs filtered by
    `testgrid.dashboard`. The kubernetes ecosystem path.
  - `source: bucket`: list the storage bucket's own job indexes (`logs/` and
    `pr-logs/directory/`). Works for any Prow instance regardless of where job
    configs live; no `testgrid.dashboard` needed. Optional `job_filters` keep
    only job names containing a substring (omit to take every job in the
    bucket, which suits a project-dedicated bucket).
- **`testgrid.dashboard`** (required only for `discovery.source: testgrid`): the
  value your jobs set in their `testgrid-dashboards` annotation, e.g.
  `"sig-release-master-blocking"`. The engine keeps every job whose annotation
  contains this name, regardless of the file path the job is defined in. Find it
  in the job's definition under `kubernetes/test-infra/config/jobs/`, or on
  testgrid. Release-branch periodics that advertise a different dashboard are
  excluded automatically.
- **`branding`** (required):
  - `title`: header text, e.g. `"My Project Prow Dashboard"`.
  - `base_path`: the Pages sub-path, which is `/` plus the repo that serves
    Pages, e.g. `/myproject-prow-ai-dashboard`. Leading slash, no trailing slash.
  - `site_url`: the full Pages URL, e.g.
    `https://my-org.github.io/myproject-prow-ai-dashboard`.
  - `source_repo`: the project's code repo, as `{owner, name}`, e.g.
    `{owner: kubernetes, name: kubernetes}`. Used to link cited source files;
    this is the code repo, not the dashboard repo.
- **`categories`** (optional): an ordered list of `{match, id, label}`, e.g.
  `{match: "conformance", id: "conformance", label: "Conformance"}`. `match` is
  a lowercase substring tested against the job name; first match wins; unmatched
  jobs go in `"other"`. Put specific rules before broad ones. Omit it to render
  one flat grid. `category_display_order` (a list of `id`s) orders the sections
  independently of match precedence.
- **`ai`** (optional): the endpoint, model, and tools for the analysis loop.
  Omit it to use the GitHub Copilot defaults, or set it to point at another
  provider or tune tools, e.g.
  `endpoint: "https://api.githubcopilot.com/chat/completions"`,
  `model: "claude-sonnet-4.5"`, `tools: [filesystem, k8s]`. All other knobs have
  defaults. See [agentic.md](agentic.md) for the full schema and
  [ai-providers.md](ai-providers.md) for provider-specific endpoint and model
  values.

### Example: a non-GCS Prow (S3 behind gcsweb)

A project on its own Prow instance, with artifacts in S3 fronted by a gcsweb
gateway and no testgrid annotations, uses the `gcsweb` provider plus bucket
discovery:

```yaml
storage:
  provider: gcsweb
  bucket: my-prow
  base: "https://gcsweb.my-project.io/s3"     # raw objects + HTML listings
  prow_base: "https://prow.my-project.io/view/s3"
discovery:
  source: bucket
  job_filters:
    - "integ-"        # keep only jobs whose name contains this (omit for all)
```

Everything downstream (build enumeration, JUnit parsing, the agentic analysis)
is identical; only the storage and discovery blocks change.

## Step 2: `prompts/system.md`

Required: the fetcher hard-errors at startup if it is missing or
whitespace-only. There is no default prompt. See
[writing-prompts.md](writing-prompts.md) for the sections worth including.

## Step 3: workflows

Two thin callers of the engine's reusable workflows. Copy the templates from
[quickstart.md](quickstart.md#step-1-pick-where-it-lives) and set `project_dir`
to match where `project.yaml` lives. The deploy cron (`*/30 * * * *` is common)
is the only field most projects adjust.

### Versioning and pinning

The `uses:` ref controls **both** the workflow and the engine code it builds
(checked out at the same commit, so they cannot drift).

```yaml
# Recommended: latest stable v1 (auto patch + minor, no breaking changes)
uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@v1

# Pin an exact release (fully frozen)
uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@v1.2.0

# Pre-release for testing
uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@v1.0.0-rc.1

# Bleeding edge (no stability guarantee)
uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
```

- Use the same ref on `reusable-clear-cache.yml`.
- `@vMAJOR` is the sweet spot: automatic fixes and features, with a deliberate
  bump only for a new major. See [releasing.md](releasing.md).
- Optional: set `min_engine_version` in `project.yaml` to warn (advisory only)
  when the pinned engine is older than your config expects.

## Step 4: pick a host repo

Both options end up at `https://<org>.github.io/<repo>/`.

**Option A: dedicated repo.**

```bash
gh repo create my-org/<repo-name> --public
git clone https://github.com/my-org/<repo-name> && cd <repo-name>
# Copy the files into the root; use project_dir: "." in the workflows.
git add -A && git commit -m "Bootstrap prow-ai-dashboard" && git push -u origin main
```

**Option B: existing repo.** Add the configs to a subdirectory plus the two
workflows under `.github/workflows/`, and set `project_dir` to that subdir.

```bash
cd path/to/existing-repo
mkdir -p dashboard/prompts .github/workflows
# project.yaml -> dashboard/, system.md -> dashboard/prompts/,
# deploy.yml + clear-cache.yml -> .github/workflows/   (project_dir: "dashboard")
git add -A && git commit -m "Add prow-ai-dashboard" && git push
```

A repo can serve only one Pages site. If the existing repo already publishes
Pages, use Option A (a dedicated repo); enabling the dashboard's deploy would
replace the existing site. Non-Pages deploy targets are not yet supported.

## Step 5: manual GitHub config

Done once by the host-repo owner; not scriptable from the engine.

```bash
# Enable Pages with the Actions build source
gh api repos/my-org/<repo-name>/pages -X POST -F build_type=workflow

# Set the AI token secret (gh prompts for the value)
gh secret set AI_TOKEN --repo my-org/<repo-name>
# Optional Slack notifications
gh secret set SLACK_WEBHOOK_URL --repo my-org/<repo-name>
```

## Step 6: first deploy + validation

```bash
gh workflow run deploy.yml --repo my-org/<repo-name>
gh run watch --repo my-org/<repo-name> --exit-status
```

After it goes green, check:

- `https://<org>.github.io/<repo>/` returns 200.
- `.../data/manifest.json` reflects your branding.
- `.../data/dashboard.json` lists your jobs (count matches Step 0).
- For a failing job, `.../data/jobs/<sanitized-job-id>.json` (the `job_id` from
  `dashboard.json`, with non-alphanumeric characters replaced) has failed
  `test_cases` with an `ai_summary` that names real symbols from your project.

If summaries read generically, add specifics to `prompts/system.md` and
redeploy. Prompt edits take effect automatically: the affected analyses re-run
on the next deploy with no cache clear. Two or three iterations is normal. (To
re-baseline everything at once, run the **Clear AI Cache** workflow first.)

## Optional: chat-completions endpoint unreachable from GitHub-hosted runners

If your endpoint is private (Azure Private Endpoint, K8s ClusterIP service,
on-prem inference), GitHub-hosted runners cannot reach it. Two options:

**Fetch locally, publish pre-fetched data.** Set `skip-fetch: true` under
`with:` in `deploy.yml` so each deploy publishes the committed
`<project_dir>/data/` instead of running the fetcher. Then run the fetcher where
the endpoint is reachable and commit the output:

```bash
cd /tmp/engine/backend && go build -o /tmp/fetcher ./cmd/fetcher
# <project_dir> is the repo root for a dedicated repo, or the subdir (e.g.
# dashboard) for an existing one; the deploy reads <project_dir>/data/.
AI_ENDPOINT="http://localhost:8000/v1/chat/completions" \
AI_TOKEN="<key or any non-empty string>" AI_MODEL="<model id>" \
  /tmp/fetcher -project-dir=<project_dir> -out=<project_dir>/data -ai

git add <project_dir>/data && git commit -m "Refresh prefetched data" && git push
```

**Self-hosted runner with cluster-internal access.** For automated runs when the
endpoint lives in a Kubernetes cluster, run the deploy on an in-cluster runner:

```yaml
uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@v1
with:
  project_dir: .
  runs-on: my-runner   # ARC runner-set name (a JSON array also works)
  ai-endpoint: http://your-svc.ns.svc.cluster.local:8000/v1/chat/completions
```

See [self-hosted-runner-in-cluster.md](self-hosted-runner-in-cluster.md) for the
full ARC install and tuning walkthrough.
