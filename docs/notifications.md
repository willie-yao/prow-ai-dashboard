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

## Project configuration

Add the email block to the consumer's `project.yaml`:

```yaml
notifications:
  email:
    enabled: true
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

Store the password in a Kubernetes Secret and expose it only to the fetcher or
worker:

```bash
kubectl -n dashboards create secret generic capz-smtp \
  --from-literal=password=<smtp-password>
```

```yaml
fetcher:
  extraEnv:
    - name: EMAIL_SMTP_PASSWORD
      valueFrom:
        secretKeyRef:
          name: capz-smtp
          key: password
```

The server does not send scheduled notifications. The fetcher CronJob or watch
worker sends them during full passes.

## State and retry behavior

Deduplication state is stored in `notification_state.json` alongside the fetcher
cache. It is preserved between runs but is not published by Pages or served by
the Kubernetes server.

State changes only after successful delivery:

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
