# prow-ai-dashboard

Reusable engine for **AI-powered Prow/TestGrid dashboards**. Provides a project-agnostic alternative to TestGrid with AI-driven failure analysis, run triage, and notifications. Each consuming project gets its own deployment, secrets, and GitHub Pages site by calling the reusable workflow shipped here.

## Consumers

| Project | Dashboard | Source |
| --- | --- | --- |
| [Cluster API Provider Azure (CAPZ)](https://github.com/kubernetes-sigs/cluster-api-provider-azure) | [willie-yao.github.io/capz-prow-ai-dashboard](https://willie-yao.github.io/capz-prow-ai-dashboard/) | [willie-yao/capz-prow-ai-dashboard](https://github.com/willie-yao/capz-prow-ai-dashboard) |
| [Cluster API (CAPI core)](https://github.com/kubernetes-sigs/cluster-api) | [willie-yao.github.io/capi-prow-ai-dashboard](https://willie-yao.github.io/capi-prow-ai-dashboard/) | [willie-yao/capi-prow-ai-dashboard](https://github.com/willie-yao/capi-prow-ai-dashboard) |

> ⚠️ **Active development.** Engine APIs (project.yaml schema, reusable workflow inputs) may still change. Pin to `@main` or a commit SHA from your consumer until a release is cut.

## How it works

Consumer repos own their `project.yaml` and `prompts/system.md`. They pin this engine's reusable workflow and pass the directory where those files live (default: repo root). The engine repo holds the shared Go + frontend code, the universal Prow base prompt, and the response-format JSON schema. It holds **no** per-project config.

```yaml
# In consumer-repo/.github/workflows/deploy.yml
jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    with:
      project_dir: .          # directory containing project.yaml + prompts/system.md
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
      SLACK_WEBHOOK_URL: ${{ secrets.SLACK_WEBHOOK_URL }}
```

The reusable workflow checks out the consumer repo (which contributes the config + prompt) and this engine repo (which contributes the code), runs the fetcher with `-project-dir=<consumer>/<project_dir>`, builds the frontend with the project's branding, and publishes the result to the **consumer repo's** GitHub Pages site via the official `actions/deploy-pages` pipeline (no `gh-pages` branch involved).

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
│ Consumer repo (e.g. capz-prow-ai-dashboard)                      │
│                                                                  │
│   project.yaml                  bucket, branding, AI endpoint    │
│   prompts/system.md             mandatory project addendum       │
│   .github/workflows/deploy.yml  ~20 lines                        │
│   secrets:  AI_TOKEN, SLACK_WEBHOOK_URL                          │
│   GitHub Pages:                 built site + cached data         │
└──────────────────────────────────────────────────────────────────┘
```

Three extension points:
- **Layer 1 — Consumer's `project.yaml`**: bucket, dashboard, branding, AI provider (Copilot, OpenAI, Azure OpenAI, Nvidia Dynamo/NIM, vLLM, Ollama, ...). See [docs/ai-providers.md](docs/ai-providers.md).
- **Layer 2 — Consumer's `prompts/system.md`**: project-specific AI knowledge. Mandatory — the fetcher hard-errors if missing when `-ai` is enabled. See [docs/writing-prompts.md](docs/writing-prompts.md).
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
full worked example. In short:

1. Create a consumer repo (any name).
2. Add `project.yaml` to its root. See [`configs/example/project.yaml`](configs/example/project.yaml) for every field.
3. Add `prompts/system.md` to its root. See [docs/writing-prompts.md](docs/writing-prompts.md) for guidance.
4. Add `.github/workflows/deploy.yml` calling this engine's `reusable-deploy.yml@main` (snippet above).
5. Add `AI_TOKEN` (and optional notification webhooks) as repo secrets.
6. Enable GitHub Pages on the consumer repo, set **Source: GitHub Actions**.

No engine PR required.

## License

[Apache 2.0](LICENSE)
Reusable engine for AI-powered Prow/TestGrid dashboards
