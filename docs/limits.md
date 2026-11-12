# Guarantees, limits and roadmap

| It promises                                                                                                    | It does not promise                                                                                          |
| -------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------ |
| A message that got `202` is not lost, even if the process is killed                                            | Exactly once. A crash or a slow answer can cause a repeat, so receivers must ignore a `webhook-id` they know |
| Every attempt is recorded, with its status, duration and failure reason                                        | A particular order. A retry can be overtaken by a newer message                                              |
| One endpoint cannot use more than its concurrency of the workers, or start attempts faster than its rate limit | That a receiver is reachable. A receiver on a private network needs the development flag                     |
| Secrets, tokens and API keys are never written to the log or the audit log                                     | Encryption of payloads. Only signing secrets are encrypted, and only if you set a key                        |
| Finished data is deleted after the retention period                                                            | That the file gets smaller. Deleted space is reused, and shrinking the file needs a `VACUUM`                 |

What this version does not do:

**Security and access**

- The operator's API key can do everything, and the audit log shows it only as `admin`, so people who share it cannot be told apart.
- A full token has the whole authority of its app. A token can be read only or can expire, and there is nothing in between. When it was last used is recorded only to within ten minutes.
- The audit entry is written after the action, not in the same transaction. A failed write is logged and counted in `bowler_audit_errors_total`, and the entry is missing.
- Without `BOWLER_ENCRYPTION_KEY`, signing secrets are stored in the clear. With it, the key is in the running program's environment, so it protects a copy of the file and not a person on the machine, and if the key is lost the secrets are lost.
- Payloads and the start of each receiver's reply are never encrypted. They are deleted after the retention period, but until then they are in the clear, so use an encrypted volume.
- No TLS and no rate limit on the API itself. Put it behind a proxy. The per-endpoint rate limit protects receivers, not the API.
- A receiver on a private network cannot be reached without the global development flag, which removes the SSRF protection. There is no per-endpoint allow list yet.
- HMAC signatures only. The specification's asymmetric `v1a` scheme is not implemented.

**Behaviour**

- An idempotency key stops protecting once its message is deleted by retention.
- A rate limit allows a burst of one second's worth after a quiet time, and holds the rate on average, to within the 250 ms poll.
- At least once, not exactly once, and no ordering guarantee.

**Scale and scope**

- One node. SQLite has one writer and one file, and there is no replication, so back the file up.
- No interface for your customers. It is an API. Tokens make such a page possible, and the page itself is not built.
- The Docker image is built and checked by CI, and has not been built by hand on the author's machine.
- Demo grade. It has had no independent security review.

The threat model, [threat-model.md](threat-model.md), lists what is protected, from whom, how, and what is not covered. [SECURITY.md](../SECURITY.md) says how to report a problem and how to deploy safely.

## Roadmap

1. Postgres, and more than one node
2. A retention period per app, and encrypting payloads
3. Named operator credentials, and the audit entry in the same transaction as the change
4. A page for your customers to manage endpoints and read their delivery log, using tokens
5. Wake the dispatcher exactly when a rate limit allows more, instead of on the next poll
6. Asymmetric `v1a` signatures, and a per-endpoint allow list for private receivers
