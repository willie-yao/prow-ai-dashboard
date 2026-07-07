# Server mode (Kubernetes-native)

The dashboard ships two deploy modes from one codebase:

- **Static (default).** The fetcher writes JSON, GitHub Actions builds the SPA,
  and GitHub Pages serves it. Public, cheap, no backend.
- **Server.** A small Go server (`backend/cmd/server`) serves the same JSON over
  HTTP alongside the inference stack, so the site can later gain stateful,
  interactive features. The static path keeps working unchanged.

Server mode is a strict superset of the static contract: it serves the exact
same `/data/*.json` files the SPA already reads, then adds a capability
descriptor the frontend uses to discover server-only features.

## Endpoints

| Path | Purpose |
| --- | --- |
| `GET /data/*` | The fetcher output tree at read parity: `manifest.json`, `dashboard.json`, `jobs/*.json`, `flakiness.json`, `search-index.json`. |
| `GET /api/capabilities` | Deploy descriptor, for example `{"mode":"server","features":{"chat":false,"actions":false}}`. |
| `GET /healthz` | Liveness and readiness probe. |
| `GET /` | The built SPA, when `-static-dir` is set, with deep-link fallback to `index.html`. |
| `POST /api/failures/{id}/create-issue` | Admin-gated: file a GitHub issue for one failure. Enabled only when actions are configured. |
| `POST /api/failures/{id}/propose-fix` | Admin-gated: draft a fix PR for one failure. |

## Capability seam

The frontend discovers its mode by probing `/api/capabilities`:

- In static Pages mode the endpoint does not exist, the probe fails, and the
  frontend stays in read-only static mode.
- In server mode the descriptor is present, and the frontend lights up only the
  features it advertises.

Interactive features are additive and gated behind this descriptor, so the same
build serves both targets. All `/data/*.json` schemas stay byte-compatible.

## Admin-gated actions

The write endpoints let an admin file an issue or draft a fix PR for a specific
failure on demand, reusing the same engines the scheduled fetch uses. They are
off unless the server is started with `-project-dir` and the `ADMIN_LOGINS`
environment variable lists at least one GitHub login. When enabled, the server
sets `features.actions: true` and the frontend shows the buttons.

Auth is per-user, not a shared operator token. The admin sends their own GitHub
PAT in the `Authorization` header; the server verifies it against GitHub,
checks the resolved login against `ADMIN_LOGINS`, and performs the write with
that token, so the issue or PR is attributed to the real user. The
`Authenticator` is a seam: the PAT check ships first and a GitHub OAuth flow can
replace it later without touching the handlers.

## Running locally

```bash
# Fetch data first (see docs/development.md), then serve it:
make serve                 # builds bin/server, serves frontend/public/data

# Or serve a self-contained build (SPA + data from one origin):
make fe-build
./bin/server -data-dir=frontend/public/data -static-dir=frontend/dist
```

Flags: `-addr` (default `:8080`), `-data-dir` (default `data`), `-static-dir`
(optional built SPA; empty serves data and API only).
