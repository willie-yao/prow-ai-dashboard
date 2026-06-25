# prow-ai-dashboard

Reusable engine for **AI-powered Prow/TestGrid dashboards**. Provides a project-agnostic alternative to TestGrid with AI-driven failure analysis, run triage, and notifications. Each project gets its own deployment, secrets, and GitHub Pages site by calling the reusable workflow shipped here from any repo it controls (a dedicated dashboard repo or a directory inside an existing one).

> ⚠️ **Active development.** Engine APIs (project.yaml schema, reusable workflow inputs) may still change. Pin to `@main` or a commit SHA from your consumer until a release is cut.

## How it works

A dashboard's deployable surface area is small: one config file (`project.yaml`), one prompt file (`prompts/system.md`), and one ~20-line workflow. These can live in a **dedicated repo** or alongside the project's existing code in an **existing repo that doesn't already publish a GitHub Pages site**. The reusable workflow takes a `project_dir:` input so the files can be in the repo root or any subdirectory.

```yaml
# In <your-repo>/.github/workflows/deploy.yml
jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    with:
      project_dir: .          # or .prow-dashboard, or wherever project.yaml lives
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
      SLACK_WEBHOOK_URL: ${{ secrets.SLACK_WEBHOOK_URL }}
```

The reusable workflow checks out the host repo (which contributes the config + prompt) and this engine repo (which contributes the code), runs the fetcher with `-project-dir=<repo>/<project_dir>`, builds the frontend with the project's branding, and publishes the result to the host repo's GitHub Pages site via the official `actions/deploy-pages` pipeline (no `gh-pages` branch involved).

For sites whose repo already uses GitHub Pages for something else (project websites, books, etc.) or whose chat-completions endpoint is private and unreachable from GitHub-hosted runners, see [docs/onboarding-a-new-project.md](docs/onboarding-a-new-project.md) for the supported escape hatches (`skip-fetch`, `runs-on` for self-hosted runners; non-Pages deploy targets are tracked as Phase J). When your AI inference stack runs inside a Kubernetes cluster, [docs/self-hosted-runner-in-cluster.md](docs/self-hosted-runner-in-cluster.md) walks through running the deploy on an in-cluster runner.

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│ This repo (prow-ai-dashboard) — engine                           │
│                                                                  │
│   backend/    Go fetcher + collectors + AI modules               │
│   frontend/   React UI (built per-project at deploy time)        │
│   docs/       quickstart.md, onboarding-a-new-project.md, ...    │
│   configs/    example/ — docs-only sample, no live config        │
│   .github/    reusable-deploy.yml, reusable-clear-cache.yml      │
└──────────────────────────────────────────────────────────────────┘
                              │ uses: @main
                              ▼
┌──────────────────────────────────────────────────────────────────┐
│ Host repo (dedicated or shared with the project's own code)      │
│                                                                  │
│   <project_dir>/project.yaml         bucket, branding, AI        │
│   <project_dir>/prompts/system.md    mandatory project addendum  │
│   .github/workflows/deploy.yml       ~20 lines                   │
│   secrets:  AI_TOKEN, SLACK_WEBHOOK_URL                          │
│   GitHub Pages:                      built site + cached data    │
└──────────────────────────────────────────────────────────────────┘
```

Three extension points:
- **Layer 1 — `project.yaml`**: bucket, dashboard, branding, AI provider (Copilot, OpenAI, Azure OpenAI, Nvidia Dynamo/NIM, vLLM, Ollama, ...). See [docs/ai-providers.md](docs/ai-providers.md). The [agentic loop](docs/agentic.md) is the only analysis path: the model browses the artifact tree on demand via function-calling tools.
- **Layer 2 — `prompts/system.md`**: project-specific AI knowledge. Mandatory — the fetcher hard-errors if missing when `-ai` is enabled. See [docs/writing-prompts.md](docs/writing-prompts.md).
- **Layer 3 — Engine collectors and AI modules** (`backend/internal/collectors/`, `backend/internal/ai/modules/`): `generic` collector + `generic | universal` AI modules. Selected by `project.yaml`.

## Local development

```bash
# Backend
make build
make test

# Frontend
make fe-install
make dev    # http://localhost:5173 with HMR

# Run fetcher against a consumer repo (point PROJECT_DIR at any directory
# containing project.yaml + prompts/system.md). Output lands in
# frontend/public/data/ which the Vite dev server serves automatically.
make fetch-data PROJECT_DIR=../your-consumer-repo

# Or invoke the binary directly for one-off runs:
./bin/fetcher \
  -project-dir=../your-consumer-repo \
  -out=frontend/public/data \
  -builds=3 -workers=5

# Add -ai (and set AI_TOKEN / AI_ENDPOINT / AI_MODEL env vars) to populate
# AI summaries. See docs/ai-providers.md for endpoint details.
```

Frontend-only iteration (no fetcher run): drop pre-built JSON from a deployed
site into `frontend/public/data/`, then `make dev`.

## Adding a project

New here? [docs/quickstart.md](docs/quickstart.md) gets one dashboard live in
~15 minutes on the opinionated happy path. For the granular reference (job
grouping, presubmits, private endpoints, every field) see
[docs/onboarding-a-new-project.md](docs/onboarding-a-new-project.md). In short,
drop these files into a repo with GitHub Pages capacity (either a brand-new
dashboard repo, or an existing repo that doesn't already publish a Pages site):

1. Add `project.yaml` somewhere in the repo. See [`configs/example/project.yaml`](configs/example/project.yaml) for every field.
2. Add `prompts/system.md` next to it. See [docs/writing-prompts.md](docs/writing-prompts.md) for guidance.
3. Add `.github/workflows/deploy.yml` calling this engine's `reusable-deploy.yml@main` (snippet above). Point `project_dir:` at wherever you put the two files above.
4. Add `AI_TOKEN` (and optional notification webhooks) as repo secrets.
5. Enable GitHub Pages on the repo, set **Source: GitHub Actions**.

No engine PR required.

## License

[Apache 2.0](LICENSE)
Reusable engine for AI-powered Prow/TestGrid dashboards
