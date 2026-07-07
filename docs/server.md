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
off unless the server is started with `-project-dir` and `AUTH_MODE` selects an
auth mechanism. When enabled, the server sets `features.actions: true` and the
frontend shows the buttons.

Two auth modes, both keeping the admin allowlist (`ADMIN_LOGINS`):

- **`oauth`** (per-user attribution): the operator registers a GitHub OAuth App.
  Admins sign in with GitHub; the server holds each admin's own OAuth token in
  an encrypted, httpOnly session cookie and performs the write as them, so the
  issue or PR is attributed to the real user. No token is ever entered in the
  browser. Needs `OAUTH_CLIENT_ID`, `OAUTH_CLIENT_SECRET`, `OAUTH_REDIRECT_URL`
  (the App's callback), and `SESSION_KEY`.
- **`proxy`** (bot attribution): an upstream SSO proxy (oauth2-proxy, IAP, ...)
  authenticates the user and passes their identity in a trusted header
  (`AUTH_PROXY_HEADER`, e.g. `X-Auth-Request-Email`); a single `BOT_TOKEN`
  performs the write. Simplest when you already run an authenticating proxy.

The `Authenticator` is a seam, so the two modes share one code path and a third
mechanism can be added without touching the handlers. Sessions are stateless
(encrypted cookie), CSRF is covered by a `SameSite=Lax` cookie plus an Origin
check, and tokens are never logged or returned to the browser.

### Auth endpoints (oauth mode)

| Path | Purpose |
| --- | --- |
| `GET /api/auth/login` | Redirect to GitHub to sign in. |
| `GET /api/auth/callback` | OAuth callback; establishes the session. |
| `GET /api/auth/user` | The signed-in admin, or 401. |
| `POST /api/auth/logout` | Clear the session. |

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
