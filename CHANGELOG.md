# Changelog

All notable changes to the prow-ai-dashboard engine are documented here. The
engine follows [Semantic Versioning](https://semver.org): consumers pin it via
`uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@<ref>`,
and the pinned ref controls both the workflow and the engine code it builds.

What bumps what:

- **MAJOR**: removing or renaming a `project.yaml` field, changing a reusable
  workflow input contract, or breaking the published data JSON schema.
- **MINOR**: a new optional config field, tool, or feature with safe defaults.
  Internal cache-version bumps (which force re-analysis on upgrade) are at least
  minor.
- **PATCH**: bug fixes, prompt tweaks, performance.

See [docs/releasing.md](docs/releasing.md) for the release process and
[docs/onboarding-a-new-project.md](docs/onboarding-a-new-project.md#versioning-and-pinning)
for how to pin a release.

## [Unreleased]

### Changed

- **Breaking: AI analysis now requires an explicit endpoint and model.** The
  engine no longer defaults to GitHub Copilot (`api.githubcopilot.com` /
  `claude-sonnet-4.5`) when `ai.endpoint` / `ai.model` are unset. When AI is
  enabled, configure both in `project.yaml` (`ai.endpoint`, `ai.model`) or via
  the `AI_ENDPOINT` / `AI_MODEL` env vars; otherwise the fetch fails fast with a
  clear error instead of silently calling Copilot. To keep prior behavior, set
  `ai.endpoint: https://api.githubcopilot.com/chat/completions` and
  `ai.model: claude-sonnet-4.5` (or the equivalent repo variables). This makes
  the engine fully provider-agnostic with no opinionated default.

### Added

- Optional **agent-proposed fix PRs** (`ai.fix_prs`): after each fetch, for a
  systemic recurring pattern with a concrete remediation, the engine drafts a
  minimal code fix and opens a **draft PR** against the source repo via
  fork-and-PR. Off by default and heavily guardrailed: the target file(s) are
  chosen from the repo's **real file tree** (keyword-ranked) so the model can't
  invent a path; **anchored search/replace** edits are applied only on an exact
  single match and bounded by `max_files`; draft-only; a dedicated `FIX_TOKEN`
  (a CLA-signed contributor PAT) authors the commit under that identity with a
  DCO `Signed-off-by`; idempotent marker dedup; and a `max_new_per_run` cap. A
  `dry_run` mode runs the full pipeline and writes proposed diffs to
  `fix_previews.json` without opening any PR. `fork: false` (default `true`)
  switches from fork-and-PR to a direct branch + same-repo PR for a source repo
  you own. The `ghpr` helper gained fork-and-PR support (fork on demand,
  cross-fork draft PR) for this. See [docs/fix-prs.md](docs/fix-prs.md).

- Optional **self-improving skills** (`ai.suggest_skills`): after each fetch,
  the engine drafts a diagnostic skill recipe for any systemic recurring pattern
  that no existing skill covers, and opens a **draft PR** adding
  `skills/<id>.yaml` to the dashboard repo for review. Off by default; enable
  with `ai.suggest_skills.enabled: true`. Reuses the configured AI provider to
  decide coverage (trigger prefilter + an LLM confirm) and draft the recipe,
  validates the draft against the skills schema before proposing, and dedupes by
  a hidden marker so a pattern is never suggested twice. Needs a `SKILL_TOKEN`
  secret (contents + pull-requests write on the dashboard repo; a dedicated
  token like `ISSUE_TOKEN`, so the deploy job's default permissions stay
  read-only). See [docs/skills.md](docs/skills.md#auto-suggesting-recipes).
- New internal `ghpr` helper extracts the onboard scaffold's one-commit
  "open a PR from a file-set" flow (GitHub Git Data API) so onboarding and skill
  suggestions share it, with seams (draft, labels, commit author, DCO sign-off)
  for a future source-repo fix-PR feature.

- New `fetcher onboard` subcommand scaffolds a new dashboard from a testgrid
  dashboard name or a storage bucket. It verifies discovery actually finds jobs,
  infers `categories` from the job names, and writes a ready-to-review scaffold
  (`project.yaml`, both workflows, a `prompts/system.md` draft, and a manual
  `CHECKLIST.md`), validating the generated config against the engine's own
  loader before writing. When `AI_TOKEN` (plus the provider's `AI_ENDPOINT` and
  `AI_MODEL`, both required since the engine assumes no default endpoint) is set,
  it drafts `prompts/system.md` from the source repo's own docs (architecture,
  where evidence lives, known transient classes); otherwise it writes a stub. By
  default it writes a local directory and makes no GitHub writes; pass
  `-open-pr` to open a scaffold PR against the dashboard repo instead (one
  commit on a new branch), using `GITHUB_TOKEN`. It runs without a clone via
  `go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest
  onboard ...`. See
  [docs/onboarding-a-new-project.md](docs/onboarding-a-new-project.md#fast-start-scaffold-it-with-onboard).
- Optional **auto-filing of GitHub issues** for the dashboard's highest-signal
  findings: systemic recurring patterns and persistent failures (≥3 consecutive
  runs). Off by default; enable with an `issues:` block in `project.yaml` plus an
  `ISSUE_TOKEN` secret (a token with `issues: write` on the target repo, which
  defaults to `branding.source_repo` but should usually point at a repo you
  control). Each finding maps to one issue, deduped by a hidden marker via local
  state plus an eviction-proof repo-side search, so the same issue is reused
  across runs rather than re-created. Recovered findings get a "recovered"
  comment (and optionally a close). See
  [docs/github-issues.md](docs/github-issues.md).

### Changed

- `AI_TOKEN` is no longer allowed to fall back to `GITHUB_TOKEN`. It is the
  credential for the configured chat-completions endpoint (a Copilot PAT, an
  OpenAI/NVIDIA key, a self-hosted placeholder, etc.) and must be set explicitly
  to enable AI analysis; a GitHub token is unrelated to most users' model
  endpoint and the Actions-provided `GITHUB_TOKEN` cannot authenticate to a model
  provider anyway. Deployed consumers already pass `AI_TOKEN`, so they are
  unaffected; only local runs that relied on the implicit fallback now need
  `AI_TOKEN` set.

## [1.0.0-beta.4] - 2026-06-26

### Added

- New job-level, cross-build **pattern analysis** (always on, no flag). After
  the per-failure analyses complete, for any job that failed in at least 3 recent
  builds the engine correlates one representative failure per failed build into a
  single verdict: do these failures share one root cause (a systemic, fixable bug
  surfacing as repeated "flakes") or are they genuinely independent? The specific
  failing test/spec may differ between builds; the pass weighs the underlying
  mechanism. Like artifact-tree seeding it is not configurable: self-gating (a
  no-op on a healthy dashboard) and cached, so it costs nothing until a job
  genuinely recurs, then one extra tool-free model call. It surfaces as a banner
  at the top of the job page, and the systemic verdicts are aggregated across all
  jobs into the landing page's **Needs Attention** box. See
  [docs/agentic.md](docs/agentic.md#pattern-analysis).
- Editing `prompts/system.md` now takes effect automatically: each analysis is
  fingerprinted with the prompt that produced it, and on the next run any failure
  whose prompt no longer matches is re-analyzed. No manual cache clear is needed.
  Re-analysis is incremental and failure-preserving (an old analysis stays
  published until its replacement succeeds), so results aren't lost while it
  catches up. The **Clear AI Cache** workflow remains available to re-baseline
  everything at once. Note: the first run after upgrading re-analyzes existing
  entries once (they predate the fingerprint), consistent with other
  cache-version bumps.

### Fixed

- Bucket-discovered jobs (`discovery.source: bucket`) now get a display title and
  category. They previously rendered as untitled cards under "Other" because the
  bucket path did not set `tab_name` or apply the project's `categories` rules
  (only the testgrid path did). The categorize logic is now shared across both
  discovery sources. The job-card title also falls back to the job name when no
  tab name is present.

### Changed

- The landing page's **Needs Attention** box is now collapsible, with its
  open/closed state remembered across visits, so a long alert list no longer
  pushes the job grid down the page.
- `storage.provider` is now required (no implicit `gcs` default), so the config
  is explicit about the backend rather than assuming a provider. Set
  `provider: gcs` for Google Cloud Storage. Consumers already setting a provider
  are unaffected.

## [1.0.0-beta.3] - 2026-06-25

### Added

- Storage is now pluggable so the engine no longer assumes Google Cloud Storage.
  A new `storage.provider` selects the backend: `gcs` (native GCS, the previous
  behavior) or `gcsweb` (any gcsweb HTTP gateway fronting a bucket, e.g. an S3
  bucket behind `gcsweb.<project>.io`). For `gcsweb`, set `storage.base` (the
  gateway) and optionally `storage.prow_base`/`storage.web_base`. Ranged reads
  are emulated for gateways without HTTP Range support.
- Pluggable job discovery via `discovery.source`: `testgrid` (default, the
  kubernetes/test-infra path) or `bucket`, which lists the artifact bucket's own
  `logs/` and `pr-logs/directory/` indexes and needs no job-config repo. Works
  for any Prow instance; optional `discovery.job_filters` scope by job-name
  substring. Together these let non-kubernetes Prow projects (e.g. Istio on S3)
  onboard with no engine changes.

### Changed

- **BREAKING (config):** the `gcs:` block is replaced by `storage:`. Migrate
  `gcs: {bucket: X}` to `storage: {provider: gcs, bucket: X}`. `testgrid.dashboard`
  is now required only when `discovery.source` is `testgrid` (the default).

## [1.0.0-beta.2] - 2026-06-24

### Added

- Release process: tag-triggered release workflow, semver tags, a moving
  `vMAJOR` alias, this changelog, and `docs/releasing.md`.
- Engine version is embedded at build time and logged at startup; an optional
  `min_engine_version` field in `project.yaml` warns when the engine is older
  than the config expects.
- Quickstart guide and a "Tuning by model tier" reference for the agentic loop.
- In-cluster self-hosted runner guide for private AI endpoints.
- AI analysis rendering: running builds show a yellow (not red) status dot;
  inline `code` spans render as monospace pills; and cited file paths link to
  their source. Source links are verified to exist at fetch time
  (`file_links` on each analysis) so a file in a different repo than the project
  is never turned into a broken link. Repo resolution is generic (project repo,
  Go vanity import via `?go-get=1`, or `owner/repo/path`) with no project- or
  ecosystem-specific knowledge in the engine. Inline links display just the
  filename, with the full path shown on hover.

### Changed

- **Single-pin engine reference**: the deploy workflow now builds the engine at
  the pinned workflow commit. The `engine-ref` input was removed. No action
  needed for consumers (none set it); `@main` callers are unaffected.

### Fixed

- Deep links no longer render a blank page on GitHub Pages (SPA fallback).
- Oversized junit failure messages and artifact-tree seeds no longer overflow
  the model context window on the first request.
- Slow chat endpoints no longer hit a fixed per-request HTTP timeout: each chat
  request is now bounded only by the per-failure `timeout` budget, so reasoning
  and self-hosted models whose decode exceeded the old 60s client cap complete
  instead of erroring out.
- A failure whose analysis could not complete (endpoint error, timeout, or a
  misconfigured run) now has its "AI analysis unavailable" summary refreshed on
  the next run instead of keeping the stale message. Errored failures are
  re-analyzed every run, so once the endpoint is healthy they converge to a real
  analysis; transient classifications and real summaries are still preserved.
