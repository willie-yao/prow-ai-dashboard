# prow-ai-dashboard

Reusable engine for **AI-powered Prow/TestGrid dashboards**: a project-agnostic
alternative to TestGrid with AI-driven failure analysis, run triage, and
notifications. Each project gets its own deployment, secrets, and GitHub Pages
site by calling the reusable workflow shipped here from any repo it controls.

> ⚠️ **Active development.** Engine APIs such as the `project.yaml` schema and
> reusable workflow inputs may still change. Pin to `@main` or a commit SHA
> until a release is cut.

## How it works

A project ships three files in a dedicated repo or a subdirectory of an existing
one: `project.yaml`, `prompts/system.md`, and a ~20-line `deploy.yml`. The host
repo must not already publish a GitHub Pages site.

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

The reusable workflow checks out the host repo for the config and prompt and
this engine for the code, runs the fetcher against `<project_dir>`, builds the
branded frontend, and publishes to the host's GitHub Pages via
`actions/deploy-pages`. For repos that already use Pages or whose AI endpoint is
private, see the escape hatches in
[onboarding](docs/onboarding-a-new-project.md) and
[in-cluster runners](docs/self-hosted-runner-in-cluster.md).

## What you configure

A dashboard is shaped by three things:

- **`project.yaml`**: bucket, dashboard, branding, AI provider, and feature
  toggles. See [`configs/example/project.yaml`](configs/example/project.yaml)
  for every field.
- **`prompts/system.md`**: project-specific AI knowledge. Mandatory; the fetcher
  hard-errors if it is missing when `-ai` is enabled.
- **Engine collectors and AI modules** in `backend/internal/collectors/` and
  `backend/internal/ai/modules/`, selected by `project.yaml`. The engine itself,
  a Go fetcher in `backend/` and a React UI in `frontend/`, is built per project
  at deploy time; you never fork it.

## Documentation

**Getting started**
- [Onboarding a new project](docs/onboarding-a-new-project.md): the single
  setup path. The `onboard` subcommand scaffolds a dashboard, then a full
  field-by-field reference covers the rest.

**Configuration & authoring**
- [AI providers](docs/ai-providers.md): point the engine at any
  OpenAI-compatible endpoint, such as Copilot, OpenAI, Azure, Dynamo/NIM, vLLM,
  or Ollama.
- [Writing prompts](docs/writing-prompts.md): author the required
  `prompts/system.md`.
- [Agentic loop](docs/agentic.md): how the model browses artifacts via
  function-calling tools, and how to tune it per model tier.

**Features**
- [GitHub issues](docs/github-issues.md): auto-file and maintain issues for the
  highest-signal failures.
- [Skills](docs/skills.md): author diagnostic recipes, and auto-suggest new
  ones for recurring patterns.

**Operations**
- [In-cluster runner](docs/self-hosted-runner-in-cluster.md): run the deploy on
  a self-hosted runner to reach a private, in-cluster AI endpoint.
- [Releasing](docs/releasing.md): cut an engine release and how consumers pin.

**Development**
- [Local development](docs/development.md): build, test, and run the fetcher
  against a consumer repo locally.

## Adding a project

See [onboarding](docs/onboarding-a-new-project.md). In short: add `project.yaml`
and `prompts/system.md` to a repo, add a `deploy.yml` calling
`reusable-deploy.yml@main` as shown above, set the `AI_TOKEN` secret, and enable
GitHub Pages with **Source: GitHub Actions**. No engine PR required.

## License

[Apache 2.0](LICENSE)
