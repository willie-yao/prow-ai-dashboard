# Quickstart

Get a live, AI-annotated Prow dashboard for one testgrid dashboard in about
15 minutes. The happy path uses **GitHub Copilot** as the model, GitHub-hosted
runners, and engine defaults for everything else. The files drop into either a
**new repo** or an **existing one**; Step 1 has commands for both.

When you want the full set of choices (job grouping, presubmits, a self-hosted
or private AI endpoint, every configurable field), graduate to
[onboarding-a-new-project.md](onboarding-a-new-project.md), which is the
granular reference. This guide deliberately skips those forks.

## What you need

- The `gh` CLI, authenticated (`gh auth status`).
- Go 1.25+ (for the local sweep, and for the free local-model option).
- An **AI endpoint**. You pick one in Step 2; the options range from a free
  model running on your laptop (no account) to a hosted API. The only hard
  requirement is OpenAI-style function calling, which the agentic loop needs.
- The **testgrid dashboard name** your project's jobs advertise in their
  `testgrid-dashboards` annotation. That single name is how the engine
  discovers your jobs; you do not list job paths.

Throughout, replace `my-org` and `myproject` with your own values.

## Step 1: pick where it lives

The dashboard is four files. They can go in a **new dedicated repo** or in an
**existing repo** that does not already publish a GitHub Pages site. The files
are identical either way; only the setup commands and two path fields differ.
Run one of the two blocks below, then continue to the file contents.

**New dedicated repo:**

```bash
gh repo create my-org/myproject-prow-ai-dashboard --public \
  --description "AI-powered dashboard for myproject E2E tests"
git clone https://github.com/my-org/myproject-prow-ai-dashboard
cd myproject-prow-ai-dashboard
mkdir -p prompts .github/workflows
# project_dir is the repo root ("."), base_path is "/myproject-prow-ai-dashboard"
```

**Existing repo:** put `project.yaml` and `prompts/` in a subdirectory so they
stay out of the way; the two workflows always live under `.github/workflows/`.

```bash
cd path/to/your-existing-repo
mkdir -p dashboard/prompts .github/workflows
# project_dir is the subdir ("dashboard"), base_path is "/<this-repo-name>"
```

Either way you end up with this layout (rooted at your chosen `project_dir`):

```
<project_dir>/                 # repo root for a dedicated repo, or "dashboard/"
├── project.yaml
└── prompts/system.md
<repo-root>/.github/workflows/
├── deploy.yml
└── clear-cache.yml
```

Now create the four files.

**`project.yaml`** (the minimum that produces a good dashboard). `base_path` and
`site_url` always reference the repo that serves Pages, which is `<repo-name>`
in both cases:

```yaml
id: myproject
name: "My Project Prow Dashboard"

testgrid:
  dashboard: "sig-foo-myproject"   # the testgrid-dashboards value your jobs set

gcs:
  bucket: "kubernetes-ci-logs"

branding:
  title: "My Project Prow Dashboard"
  base_path: "/<repo-name>"                          # e.g. "/myproject-prow-ai-dashboard"
  site_url: "https://my-org.github.io/<repo-name>"
  source_repo:
    owner: "kubernetes-sigs"
    name: "myproject"

ai:
  # endpoint + model are filled in by Step 2 (choose your AI endpoint).
  # The example below uses a free local model; swap per Step 2 for a hosted one.
  endpoint: "http://localhost:11434/v1/chat/completions"
  model: "qwen3:8b"
  tools: [filesystem, k8s]
```

Everything not listed has a sensible default: jobs render in one flat grid (no
`categories`) and periodics only. The `ai.endpoint` / `ai.model` above are a
placeholder; Step 2 walks the real options and what to set here. Small or
open-weights models also want the guardrails described in
[agentic.md "Tuning by model tier"](agentic.md#tuning-by-model-tier).

**`prompts/system.md`** (mandatory; the fetcher hard-errors if it is missing or
empty). Start small and grow it as you read real analyses:

```markdown
# My Project: AI analysis guidance

This dashboard covers myproject E2E and conformance jobs. The project
provisions Kubernetes clusters and runs upstream + project-specific tests.

## Known transient failures (classify as transient, not a real bug)

- <error pattern>: <one line on why it is a flake, e.g. an infra capacity
  retry, a startup race that self-resolves>

## Classify by root cause, not surface symptom

Diagnose the underlying cause from the logs. A generic "context deadline
exceeded" is usually a downstream symptom; find the first real error.
```

See [writing-prompts.md](writing-prompts.md) for the sections worth adding, and
the [CAPI](https://github.com/willie-yao/capi-prow-ai-dashboard/blob/main/prompts/system.md)
or [CAPZ](https://github.com/willie-yao/capz-prow-ai-dashboard/blob/main/prompts/system.md)
prompts as fuller templates.

**`.github/workflows/deploy.yml`** (set `project_dir` to `.` for a dedicated
repo, or to your subdir such as `dashboard` for an existing repo):

```yaml
name: Deploy Dashboard

on:
  schedule:
    - cron: "*/30 * * * *"
  workflow_dispatch: {}
  push:
    branches: [main]

permissions:
  contents: read
  pages: write
  id-token: write

concurrency:
  group: deploy
  cancel-in-progress: false

jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    with:
      project_dir: "."        # or "dashboard" if you used a subdir
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
```

**`.github/workflows/clear-cache.yml`** (wipes the cache so the next deploy
re-analyzes everything; you will use it after prompt edits). Use the same
`project_dir` as `deploy.yml`:

```yaml
name: Clear AI Cache

on:
  workflow_dispatch: {}

permissions:
  actions: write

jobs:
  clear:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-clear-cache.yml@main
    with:
      project_dir: "."        # or "dashboard" if you used a subdir
```

## Step 2: choose your AI endpoint

The agentic loop needs an endpoint that supports OpenAI-style function calling.
Pick one of the options below and set `ai.endpoint` / `ai.model` in
`project.yaml` to match.

### Option A: free local model (no account)

Best for trying it out. Run a small open-weights model with
[Ollama](https://ollama.com), which exposes an OpenAI-compatible API on
`localhost:11434`. Tool calling needs a ~7B-or-larger model (smaller ones do
not call tools reliably); `qwen3:8b` is a good default and fits in roughly
6 GB.

```bash
# Native install from ollama.com, or run it in Docker:
docker run -d -p 11434:11434 -v ollama:/root/.ollama --name ollama ollama/ollama
docker exec ollama ollama pull qwen3:8b
```

```yaml
# project.yaml
ai:
  endpoint: "http://localhost:11434/v1/chat/completions"
  model: "qwen3:8b"
  tools: [filesystem, k8s]
  # small model: keep an investigation bar and repair shallow/punt answers
  min_tool_calls: 3
  min_gcs_bytes: 200000
  critique:
    enabled: true
```

A model on your laptop is not reachable from GitHub's hosted runners, so this
option does not auto-deploy. Use it to preview locally (build the fetcher as in
Step 3, run it with `-ai` against the local endpoint writing into the engine's
`frontend/public/data`, then `make dev` in the engine checkout to view). When
you are happy, publish the prebuilt `data/` with the `skip-fetch` flow in
[onboarding-a-new-project.md](onboarding-a-new-project.md#optional-ai-endpoint-unreachable-from-github-hosted-runners).
With this option you can skip the `AI_TOKEN` secret in Step 4; set it to any
non-empty placeholder (e.g. `ollama`) for local runs.

### Option B: hosted API (auto-deploys from GitHub Actions)

A hosted endpoint is reachable from GitHub's runners, so the scheduled deploy
refreshes the dashboard with no machine of yours involved. Two common choices:

**OpenAI** (public, pay per token; a first fetch is typically cents to a few
dollars):

```yaml
ai:
  endpoint: "https://api.openai.com/v1/chat/completions"
  model: "gpt-4o-mini"     # public, small, supports tool calling
  tools: [filesystem, k8s]
```

`AI_TOKEN` is your OpenAI API key.

**GitHub Copilot** (uses your GitHub account; `AI_TOKEN` is a fine-grained PAT
with the `copilot_chat` permission):

```yaml
ai:
  endpoint: "https://api.githubcopilot.com/chat/completions"
  model: "gpt-4o"          # or another model your Copilot plan exposes
  tools: [filesystem, k8s]
```

Copilot requires a subscription. Individuals can use the free Copilot tier, but
it has a limited monthly request allowance, and a full cold fetch (one
investigation per failure) can consume a large share of it; organizations need
paid Copilot licenses. Treat it as metered, not free.

For other providers (Azure OpenAI, Nvidia Dynamo / NIM, self-hosted vLLM) see
[ai-providers.md](ai-providers.md). Smaller hosted models benefit from the same
guardrails as Option A; see
[agentic.md "Tuning by model tier"](agentic.md#tuning-by-model-tier).

## Step 3: confirm job discovery (optional, ~2 min)

Before pushing, prove the engine finds your jobs locally. Skip this if you are
confident in the dashboard name; it just shortens the feedback loop.

```bash
git clone https://github.com/willie-yao/prow-ai-dashboard /tmp/engine
cd /tmp/engine/backend && go build -o /tmp/fetcher ./cmd/fetcher

cd /path/to/your-repo
export GITHUB_TOKEN=$(gh auth token)        # avoids the 60/hr anonymous limit
/tmp/fetcher -project-dir=. -ai=false -builds=1   # use your subdir instead of "."
python3 -c "import json; print(len(json.load(open('data/dashboard.json'))['jobs']), 'jobs')"
rm -rf data                                  # don't commit the sweep output
```

A non-zero job count means your `testgrid.dashboard` is correct. Zero means the
name does not match any job's `testgrid-dashboards` annotation; fix it before
deploying.

## Step 4: push, set the secret, enable Pages

Replace `my-org/<repo-name>` with the repo you are deploying from. **Option A
(local model) users:** you can skip the `AI_TOKEN` secret here and publish via
the `skip-fetch` flow instead of the scheduled deploy (see Step 2); the Pages
enablement still applies.

```bash
git add -A && git commit -m "Add myproject prow-ai-dashboard"
git push   # add "-u origin main" if this is a fresh repo

# AI token secret (gh prompts for the value)
gh secret set AI_TOKEN --repo my-org/<repo-name>

# Enable GitHub Pages with the Actions build source
gh api repos/my-org/<repo-name>/pages -X POST -F build_type=workflow
```

If you added the dashboard to an existing repo that **already** publishes a
Pages site, the line above replaces it. In that case use a dedicated repo
instead, or see the host-repo options in
[onboarding-a-new-project.md](onboarding-a-new-project.md#step-4-pick-a-host-repo).

## Step 5: deploy and watch

The push already triggered a deploy. To run one on demand and follow it:

```bash
gh workflow run deploy.yml --repo my-org/<repo-name>
gh run watch --repo my-org/<repo-name> --exit-status
```

The first cold run analyzes every recent failure, so it takes a few minutes.

## Step 6: verify the live site

```
https://my-org.github.io/<repo-name>/
```

Spot-check that the AI is grounded, not generic: open a failing job, expand a
failed test, and confirm the analysis names real symbols from your project
(controllers, custom resources, specific error strings) rather than "the build
failed during a test." If it reads generically, your `prompts/system.md` needs
more project specifics: edit it, run the **Clear AI Cache** workflow, and
redeploy. Two or three prompt iterations is normal.

## Next steps

- **Tune the model.** Pointing at a smaller or open-weights endpoint? See
  [agentic.md "Tuning by model tier"](agentic.md#tuning-by-model-tier) for the
  guardrails (investigation floors, critique gate) and copy-paste presets.
- **Group jobs, add presubmits, or use a private endpoint.** All covered field
  by field in [onboarding-a-new-project.md](onboarding-a-new-project.md).
- **Swap the AI provider** (OpenAI, Azure OpenAI, Nvidia Dynamo, vLLM, Ollama):
  [ai-providers.md](ai-providers.md).
- **Sharpen the prompt:** [writing-prompts.md](writing-prompts.md).
