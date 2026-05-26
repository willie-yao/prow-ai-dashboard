# prow-ai-dashboard

Reusable engine for **AI-powered Prow/TestGrid dashboards**. Provides a project-agnostic alternative to TestGrid with AI-driven failure analysis, run triage, and notifications. Each consuming project gets its own deployment, secrets, and GitHub Pages site by calling the reusable workflow shipped here.

> ⚠️ **Active development.** Currently being extracted from [capz-prow-dashboard](https://github.com/willie-yao/capz-prow-dashboard). Not yet stable. The first production consumer will be [capz-prow-ai-dashboard](https://github.com/willie-yao/capz-prow-ai-dashboard).

## How it works

Consumer repos pin this engine's reusable workflow and provide only secrets + a config name. All Go code, frontend code, prompts, and per-project configs live here.

```yaml
# In consumer-repo/.github/workflows/deploy.yml
jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    with:
      config: capz
    secrets:
      ai_token: ${{ secrets.AI_TOKEN }}
      slack_webhook: ${{ secrets.SLACK_WEBHOOK_URL }}
```

The reusable workflow checks out this repo, builds the fetcher, runs it with `configs/<config>/project.yaml`, builds the frontend with the project's branding, and commits the result to the **consumer repo's** `gh-pages` branch.

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│ This repo (prow-ai-dashboard)                                    │
│                                                                  │
│   backend/    Go fetcher + collectors + AI modules               │
│   frontend/   React UI (built per-project at deploy time)        │
│   configs/    project.yaml + prompts per onboarded project       │
│   .github/    reusable-deploy.yml                                │
└──────────────────────────────────────────────────────────────────┘
                              │ uses: @main
                              ▼
┌──────────────────────────────────────────────────────────────────┐
│ Consumer repo (e.g. capz-prow-ai-dashboard)                      │
│                                                                  │
│   .github/workflows/deploy.yml  (~20 lines)                      │
│   secrets:  AI_TOKEN, SLACK_WEBHOOK_URL                          │
│   gh-pages branch:  built site + cached data                     │
└──────────────────────────────────────────────────────────────────┘
```

Three extension points:
- **Layer 1 — `configs/<id>/project.yaml`**: bucket, dashboard, branding, AI endpoint.
- **Layer 2 — Artifact collectors** (`backend/internal/collectors/`): `generic | capi | kubernetes`. Selected by config.
- **Layer 3 — AI modules** (`backend/internal/ai/modules/`): project-shape-specific prompt + evidence selection.

## Local development

```bash
# Backend
make build
make test

# Frontend
make fe-install
make fe-build

# Run fetcher against a config (will exist after Phase A)
./bin/fetcher -config=configs/capz/project.yaml -out=frontend/public/data
```

## Adding a project

Once Phase C lands:

1. PR this repo adding `configs/yourproject/project.yaml` + `prompts/system.md`.
2. Create a small consumer repo with the workflow snippet above.
3. Add `AI_TOKEN` (and optional notification webhooks) as secrets in your consumer repo.
4. Enable GitHub Pages on the consumer repo.

See `docs/adding-a-project.md` (coming in Phase F).

## License

[Apache 2.0](LICENSE)
Reusable engine for AI-powered Prow/TestGrid dashboards
