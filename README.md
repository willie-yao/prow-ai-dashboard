# prow-ai-dashboard

Reusable engine for **AI-powered Prow/TestGrid dashboards**. Provides a project-agnostic alternative to TestGrid with AI-driven failure analysis, run triage, and notifications. Each project gets its own deployment, secrets, and GitHub Pages site by calling the reusable workflow shipped here from any repo it controls (a dedicated dashboard repo or a directory inside an existing one).

## Consumers

| Project | Dashboard | Source |
| --- | --- | --- |
| [Cluster API Provider Azure (CAPZ)](https://github.com/kubernetes-sigs/cluster-api-provider-azure) | [willie-yao.github.io/capz-prow-ai-dashboard](https://willie-yao.github.io/capz-prow-ai-dashboard/) | [willie-yao/capz-prow-ai-dashboard](https://github.com/willie-yao/capz-prow-ai-dashboard) |
| [Cluster API (CAPI core)](https://github.com/kubernetes-sigs/cluster-api) | [willie-yao.github.io/capi-prow-ai-dashboard](https://willie-yao.github.io/capi-prow-ai-dashboard/) | [willie-yao/capi-prow-ai-dashboard](https://github.com/willie-yao/capi-prow-ai-dashboard) |

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

For sites whose repo already uses GitHub Pages for something else (project websites, books, etc.) or whose AI endpoint is private and unreachable from GitHub-hosted runners, see [docs/onboarding-a-new-project.md](docs/onboarding-a-new-project.md) for the supported escape hatches (`skip-fetch`, `runs-on` for self-hosted runners; non-Pages deploy targets are tracked as Phase J).

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│ This repo (prow-ai-dashboard) — engine                           │
│                                                                  │
│   backend/    Go fetcher + collectors + AI modules               │
│   frontend/   React UI (built per-project at deploy time)        │
│   docs/       writing-prompts.md, ai-providers.md, ...           │
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
- **Layer 1 — `project.yaml`**: bucket, dashboard, branding, AI provider (Copilot, OpenAI, Azure OpenAI, Nvidia Dynamo/NIM, vLLM, Ollama, ...). See [docs/ai-providers.md](docs/ai-providers.md). Optional [agentic mode](docs/agentic.md) lets the model browse the artifact tree on demand via function-calling tools instead of relying on the curator's pre-fetched evidence.
- **Layer 2 — `prompts/system.md`**: project-specific AI knowledge. Mandatory — the fetcher hard-errors if missing when `-ai` is enabled. See [docs/writing-prompts.md](docs/writing-prompts.md).
- **Layer 3 — Engine collectors and AI modules** (`backend/internal/collectors/`, `backend/internal/ai/modules/`): `generic | capi`. Selected by `project.yaml`.

## Local development

```bash
# Backend
make build
make test

# Frontend
make fe-install
make fe-build

# Run fetcher against a project directory containing project.yaml + prompts/system.md
./bin/fetcher -project-dir=configs/example -out=frontend/public/data
```

## Adding a project

See [docs/onboarding-a-new-project.md](docs/onboarding-a-new-project.md) for the
full worked example. In short, drop these files into a repo with GitHub Pages
capacity (either a brand-new dashboard repo, or an existing repo that doesn't
already publish a Pages site):

1. Add `project.yaml` somewhere in the repo. See [`configs/example/project.yaml`](configs/example/project.yaml) for every field.
2. Add `prompts/system.md` next to it. See [docs/writing-prompts.md](docs/writing-prompts.md) for guidance.
3. Add `.github/workflows/deploy.yml` calling this engine's `reusable-deploy.yml@main` (snippet above). Point `project_dir:` at wherever you put the two files above.
4. Add `AI_TOKEN` (and optional notification webhooks) as repo secrets.
5. Enable GitHub Pages on the repo, set **Source: GitHub Actions**.

No engine PR required.

## License

[Apache 2.0](LICENSE)
Reusable engine for AI-powered Prow/TestGrid dashboards
