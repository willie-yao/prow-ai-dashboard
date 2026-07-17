# Email notifications

The fetcher can send SMTP email alerts for persistent test failures, changed
failure modes, and recoveries. Notifications are optional and run during the
full fetch pass or the watch worker's reconcile pass.

## Alert behavior

A test enters the persistent set after at least three consecutive failures. The
notifier sends:

- One email when a persistent failure first appears.
- Another email if the latest error hash changes.
- No repeated email while the same error continues.
- A recovery email when the test leaves the persistent set.

Failure emails include the project, job, test, consecutive count, latest error,
available AI root cause or summary, dashboard link, and Prow link. Each message
contains plain-text and HTML alternatives.

When `action_links: true`, the notifier also sends one pattern-level email for
each newly observed systemic recurring root cause. That email includes inert
**Review issue draft** and **Review fix proposal** links. Pattern emails are
deduplicated by the stable pattern id.

## Project configuration

Add the email block to the consumer's `project.yaml`:

```yaml
notifications:
  email:
    enabled: true
    action_links: false
    from: "Prow AI Dashboard <prow-dashboard@example.com>"
    to:
      - "ci-team@example.com"
    smtp:
      host: "smtp.example.com"
      port: 587
      username: "prow-dashboard@example.com"
      tls: starttls
```

The consumer config is strict. When email is enabled, `from`, at least one `to`
recipient, and `smtp.host` are required. Sender and recipient values must be
valid email addresses.

Use a team distribution list rather than personal addresses when `project.yaml`
is stored in a public repository. Notification settings are omitted from the
published `manifest.json`, but the source YAML itself remains visible.

## Action links for maintainers

Action links are opt-in and only work with a Kubernetes-native server that has
admin actions enabled:

```yaml
notifications:
  email:
    enabled: true
    action_links: true
    # from, to, and smtp omitted here
```

The links contain only the public pattern id, requested action, and dashboard
URL. They contain no GitHub token, SMTP password, preview token, or authorization
data.

Opening a link performs an inert GET. After GitHub OAuth or proxy authentication,
the dashboard shows a second **Generate draft** prompt. Only that explicit click
creates a persistent asynchronous request. The user can leave the page while the
server generates the draft. When server-side SMTP is configured, the dashboard
emails a draft-ready review link. The requesting maintainer signs in, reviews the
exact issue or diff, and confirms it before the dashboard writes to GitHub.

This extra step is intentional. Email security scanners and forwarded messages
must not be able to start generation or create an issue or PR merely by opening
a URL.

Do not enable `action_links` for a static Pages deployment. Pages has no action
API or administrator authentication. Fix proposal links also require a server
image that contains `opencode` and git.

## Inbound email replies

Kubernetes-native deployments may accept replies through a trusted inbound mail
gateway. The dashboard does not expose an SMTP server and does not parse raw
MIME. Your mail provider or gateway receives the message, validates the sender,
extracts plain text, and forwards a normalized JSON request to the dashboard.

Enable the feature in `project.yaml`:

```yaml
notifications:
  email:
    enabled: true
    action_links: true
    from: "Prow AI Dashboard <prow-dashboard@example.com>"
    to: ["ci-team@example.com"]
    smtp:
      host: "smtp.example.com"
      username: "prow-dashboard@example.com"
      tls: starttls
    inbound:
      enabled: true
      reply_to: "{token}@replies.example.com"
      maintainers:
        willie-yao: "william@example.com"
```

`reply_to` must be an address template with exactly one `{token}` placeholder in
the local part. The receiving domain must route every generated local part to
the gateway. `maintainers` maps the authenticated sender address to the server admin
identity that owns the resulting action request. In OAuth mode this is the
GitHub login; in proxy mode it is the proxy identity. Every mapped identity must
also be present in the server's `ADMIN_LOGINS` allowlist. These addresses live in
`project.yaml`, so use the same care as the public `to` recipient list.

The worker adds signed Reply-To addresses to systemic-pattern emails. The server
adds them to draft-ready emails. Both processes must receive the same
`EMAIL_REPLY_TOKEN_SECRET`. The server also requires
`EMAIL_INBOUND_WEBHOOK_SECRET` to authenticate the mail gateway. Generate
independent secrets with at least 32 characters:

```bash
openssl rand -base64 32
```

The gateway sends:

```http
POST /api/email/inbound
Authorization: Bearer <EMAIL_INBOUND_WEBHOOK_SECRET>
Content-Type: application/json
```

```json
{
  "message_id": "<provider-message-id@example.com>",
  "from": "Maintainer <william@example.com>",
  "recipient": "<signed-token>@replies.example.com",
  "text": "issue:\nMention the IPv6 impact.",
  "authenticated": true
}
```

Gateway requirements:

- Preserve the envelope recipient containing the signed token.
- Supply a stable message id so retries are idempotent.
- Set `authenticated: true` only after the sender passes the gateway's trusted
  sender-authentication policy, such as SPF, DKIM, and DMARC validation.
- Send only the new plain-text reply, or include quoted content after a standard
  reply marker. The dashboard removes common quoted-message and signature lines.
- Retry 5xx responses. Treat 2xx as accepted and 4xx as a permanent rejection.
- Keep the bearer secret out of URLs and provider logs.

Reply behavior:

- Reply to a systemic-pattern email with `issue` or `fix` on the first line.
  Optional instructions may follow after a colon or on later lines.
- Reply to a draft-ready email with additional instructions to regenerate that
  same draft.
- `confirm`, `approve`, `post`, `send`, and similar authorization replies are
  rejected. Email never confirms or writes to GitHub.
- The draft-ready review link still requires the mapped GitHub login to sign in,
  review the exact persisted content, and confirm it in the dashboard.

Pattern reply tokens expire after seven days. Draft-ready reply tokens expire
with their 24-hour action request. Processed message ids are stored only as
SHA-256 hashes in non-public `action_request_state.json` to prevent duplicate
generation when the gateway retries.

For private source repositories, provide a read-only `GITHUB_READ_TOKEN` to the
server. It is used only while generating an email-requested draft. Final issue or
PR creation uses the authenticated dashboard user's token, or the configured bot
identity in proxy mode. The write-scoped `BOT_TOKEN` is never used for draft
generation.

## SMTP security modes

`smtp.tls` accepts:

| Value | Behavior | Default port |
| --- | --- | --- |
| `starttls` | Connect in plaintext, require STARTTLS, then authenticate and send over TLS. This is the default. | 587 |
| `tls` | Establish implicit TLS before speaking SMTP. | 465 |
| `none` | Use an unauthenticated plaintext relay. This must be selected explicitly. | 25 |

Authenticated SMTP is rejected when `tls: none` so credentials are never sent
over plaintext. TLS validates the relay certificate and requires TLS 1.2 or
newer. There is no insecure certificate-skip option.

## GitHub Actions and Pages

When `smtp.username` is configured, pass the password through the reusable
workflow:

```yaml
jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    with:
      project_dir: .
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
      EMAIL_SMTP_PASSWORD: ${{ secrets.EMAIL_SMTP_PASSWORD }}
```

Create the secret once:

```bash
gh secret set EMAIL_SMTP_PASSWORD --repo my-org/my-dashboard
```

The SMTP relay must be reachable from the selected runner. For a private relay,
use a self-hosted runner, the Kubernetes-native deployment, or an externally
reachable trusted relay.

An unauthenticated relay leaves `smtp.username` empty and does not need the
password secret.

## Kubernetes-native deployment

Store the password in a Kubernetes Secret. Expose it to the fetcher or worker
for scheduled alerts, and to the server for asynchronous draft-ready emails:

```bash
kubectl -n dashboards create secret generic capz-smtp \
  --from-literal=password=<smtp-password>
```

```yaml
fetcher:
  extraEnv: &smtpEnv
    - name: EMAIL_SMTP_PASSWORD
      valueFrom:
        secretKeyRef:
          name: capz-smtp
          key: password
server:
  extraEnv: *smtpEnv
```

The fetcher sends scheduled failure alerts. The server sends draft-ready emails
for asynchronous action requests. Ready requests and their exact reviewed drafts
are persisted in non-public `action_request_state.json` for 24 hours. Pending
requests interrupted by a server restart are marked failed and can be recreated.

## State and retry behavior

Deduplication state is stored in `notification_state.json` alongside the fetcher
cache. It is preserved between runs but is not published by Pages or served by
the Kubernetes server.

State tracks both persistent-test alerts and action-enabled systemic-pattern
alerts. State changes only after successful delivery:

- A failed new or changed-failure email is retried on the next full pass.
- A failed recovery email keeps its state entry and is retried.
- Delivery failures are logged and do not fail the fetch or block other side
  effects.

The email implementation uses the state channel `email-v1`. On the first run
after upgrading from Slack notifications, old channel-less state is reset. Each
currently persistent failure therefore receives one initial email.

## Troubleshooting

- **`email disabled` in logs:** add `notifications.email.enabled: true`.
- **`EMAIL_SMTP_PASSWORD is unset`:** set the secret, or remove `smtp.username`
  when using an unauthenticated relay.
- **Relay does not advertise STARTTLS:** configure the relay for STARTTLS or use
  the correct `tls` mode. The engine does not downgrade automatically.
- **Certificate error:** use a certificate trusted by the runner or pod. The
  engine does not support insecure certificate skipping.
- **Connection timeout:** verify DNS, firewall rules, port, and reachability from
  the execution environment.
- **Repeated attempts after an error:** expected behavior. Failed deliveries do
  not advance notification state.

## Implementation reference

- `backend/internal/notify/notify.go`: triggers, deduplication, recovery, state.
- `backend/internal/notify/email.go`: plain-text and HTML rendering.
- `backend/internal/notify/smtp.go`: SMTP, authentication, TLS, MIME delivery.
- `backend/internal/fetcher/fetcher.go`: configuration and side-effect wiring.
