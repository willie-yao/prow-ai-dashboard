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
