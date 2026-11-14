# Design decisions

Each record says what was decided, why, and what it costs. Newest last.

| #    | Decision                                                                                                                         |
| ---- | -------------------------------------------------------------------------------------------------------------------------------- |
| 0001 | [Delivery is at least once, and a lease decides who works on a delivery](0001-at-least-once-with-leases.md)       |
| 0002 | [Webhooks are signed as the Standard Webhooks specification says](0002-standard-webhooks-signing.md)              |
| 0003 | [Customer URLs are checked when registered and again when a connection is made](0003-ssrf-at-connection-time.md)  |
| 0004 | [One SQLite file and one static binary](0004-sqlite-single-binary.md)                                             |
| 0005 | [One receiver cannot hold up the rest, and a receiver that is fixed can catch up](0005-isolation-and-recovery.md) |
| 0006 | [Results are written in batches by one writer](0006-batched-result-writes.md)                                     |
| 0007 | [An endpoint can be limited to a number of attempts per second](0007-per-endpoint-rate-limit.md)                  |
| 0008 | [Finished data is deleted after a retention period](0008-retention.md)                                            |
| 0009 | [An app can be given a token that works on that app only, and what is done is recorded](0009-app-tokens-and-audit-log.md) |
| 0010 | [Signing secrets are encrypted in the database when a key is given](0010-encrypted-signing-secrets.md) |
| 0011 | [The audit entry is written in the same transaction as the change](0011-audit-entry-in-the-same-transaction.md) |
