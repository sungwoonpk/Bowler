# Features

What Bowler does, in groups. Each links to the decision record that explains it and what it costs.

**Delivery**

- **Reliable.** At least once. Retries follow a schedule of about a day, with jitter, and honour `Retry-After`.
- **Isolated.** An endpoint has a concurrency (1 to 20). The queue takes only as many deliveries from an endpoint as it has free slots, so a backlog behind one slow receiver cannot starve the rest. [0005](decisions/0005-isolation-and-recovery.md)
- **Rate limited.** An endpoint can also be limited to a number of attempts per second (1 to 1000). The limit is kept in the database, so a restart does not reset it. It applies to retries and to `recover`, so replaying a long outage does not flood a receiver that has just been fixed. [0007](decisions/0007-per-endpoint-rate-limit.md)
- **Replayable.** Replay one delivery, or `recover` a whole endpoint.
- **Idempotent to send.** An `Idempotency-Key` makes retrying a request whose answer you never got safe.
- **A clear failure vocabulary.** Every failed attempt says why: `timeout`, `dns`, `connection_refused`, `tls`, `blocked_address`, `redirect`, `http_4xx`, `http_5xx`, `gone`, `rate_limited`.

**Security**

- **Signing.** `webhook-id`, `webhook-timestamp` and `webhook-signature`, as Standard Webhooks describes. Rotating a secret sends two signatures for a grace period, so receivers can switch without losing a message. The `signature` package has no dependencies. [0002](decisions/0002-standard-webhooks-signing.md)
- **SSRF protection.** https only, and no private, loopback or metadata addresses, checked when a URL is registered and again on the address actually dialled. Redirects are not followed and proxies from the environment are ignored. The operator can open named private ranges for receivers on their own network with `--allow-cidr`, and never a metadata address. [0003](decisions/0003-ssrf-at-connection-time.md), [0012](decisions/0012-operator-allow-list-for-private-receivers.md)
- **Encrypted signing secrets.** Set `BOWLER_ENCRYPTION_KEY` and the secrets are sealed with AES-256-GCM in the database file, so a copy of the file or a backup cannot forge webhooks. Turning it on, rotating the key and checking the key all happen in one pass at start. [0010](decisions/0010-encrypted-signing-secrets.md)
- **Tokens for one app.** The operator's key can make a token for an app. It is shown once and only its hash is stored. It works on that app and no other, another app looks the same as one that does not exist, and it can be revoked at once. A token can be **read only** (it cannot change anything, nor read a signing secret) and can **expire**. That is what a page for your customers needs, since such a page cannot hold a key that reads everyone's data. [0009](decisions/0009-app-tokens-and-audit-log.md)
- **An audit log.** Endpoint changes, token creation and revocation, every read or rotation of a signing secret, `recover` and replay are recorded with who did them (the operator, or a token), the resource and the request id. Updates list the fields changed, never the values. An entry is written in the same transaction as the change, so there is no change without one. [0011](decisions/0011-audit-entry-in-the-same-transaction.md)

**Data**

- **Retention.** Finished messages are deleted with their deliveries and attempts after 30 days, oldest first, in small transactions that never hold the queue up. A message that still has work to do is never deleted. The audit log has its own period, 365 days. [0008](decisions/0008-retention.md)

**Operation**

- **Observable.** Structured JSON logs with request ids, Prometheus metrics, `/healthz` and `/readyz`.
- **Orderly shutdown.** It stops taking work, lets attempts finish for a drain time, and hands back what it cut off, uncounted. A `healthcheck` command serves containers with no shell.
- **Small.** One static binary of about 16 MB for Linux, macOS and Windows, and one SQLite file. The Docker image is distroless and runs as an unprivileged user. [0004](decisions/0004-sqlite-single-binary.md)
