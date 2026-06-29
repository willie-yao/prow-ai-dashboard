# prow-ai-dashboard

Reusable engine for **AI-powered Prow/TestGrid dashboards**: a project-agnostic
alternative to TestGrid with AI-driven failure analysis, run triage, and
notifications. Each project gets its own deployment, secrets, and GitHub Pages
site by calling the reusable workflow shipped here from any repo it controls.

> ⚠️ **Active development.** Engine APIs (`project.yaml` schema, reusable
> workflow inputs) may still change. Pin to `@main` or a commit SHA until a
> release is cut.

## How it works

A project ships three files — `project.yaml`, `prompts/system.md`, and a
~20-line `deploy.yml` — in a dedicated repo or a subdirectory of an existing one
(as long as that repo doesn't already publish a GitHub Pages site).

```yaml
# <your-repo>/.github/workflows/deploy.yml
jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    with:
      project_dir: .          # wherever project.yaml lives
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
      SLACK_WEBHOOK_URL: ${{ secrets.SLACK_WEBHOOK_URL }}
```

The reusable workflow checks out the host repo (config + prompt) and this engine
(code), runs the fetcher against `<project_dir>`, builds the branded frontend,
and publishes to the host's GitHub Pages via `actions/deploy-pages`. For repos
that already use Pages or whose AI endpoint is private, see the escape hatches in
[onboarding](docs/onboarding-a-new-project.md) and
[in-cluster runners](docs/self-hosted-runner-in-cluster.md).

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│ This repo (prow-ai-dashboard) — engine                           │
│                                                                  │
│   backend/    Go fetcher + collectors + AI modules               │
│   frontend/   React UI (built per-project at deploy time)        │
│   docs/       onboarding-a-new-project.md, ai-providers.md ...   │
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

- **`project.yaml`** — bucket, dashboard, branding, AI provider, features.
- **`prompts/system.md`** — project-specific AI knowledge. Mandatory; the
  fetcher hard-errors if it is missing when `-ai` is enabled.
- **Engine collectors and AI modules** (`backend/internal/collectors/`,
  `backend/internal/ai/modules/`) — selected by `project.yaml`.

## Documentation

**Getting started**
- [Onboarding a new project](docs/onboarding-a-new-project.md) — the single
  setup path; the `onboard` subcommand scaffolds a dashboard, then a full
  field-by-field reference.

**Configuration & authoring**
- [AI providers](docs/ai-providers.md) — point the engine at any
  OpenAI-compatible endpoint (Copilot, OpenAI, Azure, Dynamo/NIM, vLLM, Ollama).
- [Writing prompts](docs/writing-prompts.md) — author the required
  `prompts/system.md`.
- [Agentic loop](docs/agentic.md) — how the model browses artifacts via
  function-calling tools, and how to tune it per model tier.

**Features**
- [GitHub issues](docs/github-issues.md) — auto-file and maintain issues for the
  highest-signal failures.
- [Skills](docs/skills.md) — author diagnostic recipes, and auto-suggest new
  ones for recurring patterns.

**Operations**
- [In-cluster runner](docs/self-hosted-runner-in-cluster.md) — run the deploy on
  a self-hosted runner to reach a private, in-cluster AI endpoint.
- [Releasing](docs/releasing.md) — cut an engine release and how consumers pin.

## Adding a project

See [onboarding](docs/onboarding-a-new-project.md). In short: add `project.yaml`
and `prompts/system.md` to a repo, add a `deploy.yml` calling
`reusable-deploy.yml@main` (snippet above), set the `AI_TOKEN` secret, and enable
GitHub Pages with **Source: GitHub Actions**. No engine PR required.

## Local development

```bash
make build && make test              # backend
make fe-install && make dev          # frontend at http://localhost:5173 (HMR)

# Run the fetcher against a consumer repo (a dir with project.yaml +
# prompts/system.md). Output lands in frontend/public/data/, which the dev
# server serves. Add -ai with AI_TOKEN / AI_ENDPOINT / AI_MODEL for summaries.
make fetch-data PROJECT_DIR=../your-consumer-repo
```

Frontend-only iteration: drop pre-built JSON from a deployed site into
`frontend/public/data/`, then `make dev`.

## License

[Apache 2.0](LICENSE)
