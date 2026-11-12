# 0008. Finished data is deleted after a retention period

Status: accepted

## Context

Message payloads often carry customer data, and the first version kept every one for ever. That is a privacy problem
(data kept longer than anyone needs it) and an operating problem (one SQLite file that only grows). The threat model listed
it as a gap.

## Decision

- `--retention` (default **30 days**, at least 24 hours, `0` to keep everything) says how long finished data is kept.
- Whole **messages** are deleted, and the foreign keys carry the delete to their deliveries and attempts. A delivery is
  never deleted alone, so history is never left with half a story.
- A message is deleted only when **all of this** is true:
  - it was created before the cutoff,
  - none of its deliveries is pending or in flight,
  - none of its deliveries finished after the cutoff.

  The last rule matters: a delivery that was retried for a day, or replayed by a person yesterday, keeps its message.
  Without it, old data could disappear in the middle of work on it.
- An endpoint that was deleted before the cutoff is removed once nothing refers to it.
- A **janitor** sweeps once at start and then every hour. A sweep deletes the **oldest first**, **500 messages per
  transaction**, with 50 ms between transactions, so the write lock is never held for long and the queue keeps running.
  It stops at once when the program shuts down, and what was deleted stays deleted.
- A minimum of 24 hours is enforced, so a setting cannot delete history faster than the retry schedule, which stretches
  over about a day, can finish.
- `bowler_retention_deleted_total{kind}` and `bowler_retention_errors_total` show what it does.
- `messages_by_created` is an index added for this, so finding the oldest messages does not read the rest.

## Costs

- **An idempotency key stops protecting after retention.** The key lives with the message. After the message is purged, the
  same key is accepted as new. A caller who retries a request must do so within the retention period, which for 30 days is
  generous. This is written in the API description.
- **Deleted rows do not shrink the file.** SQLite reuses the space for new data, and the file only gets smaller after a
  `VACUUM`, which needs an exclusive lock and extra disk space. Bowler does not run it. The file stops growing, which
  is what matters.
- **The default is a decision for the user.** 30 days suits most. A customer with a legal reason to keep delivery logs
  longer sets it higher, or to 0 and exports first. There is no export yet.
- Backups taken before a purge still hold the data. Retention applies to the live file.

## Alternatives considered

- **Delete deliveries and attempts but keep messages.** A message with no deliveries is of little use, and the payload is the
  part that holds personal data.
- **Delete only the payload and keep a stub.** Better for audit, and more to build and to explain. Not needed yet.
- **A per-app retention.** A customer's own policy is a real need, and it asks for settings that are not built. One
  global value comes first.
- **Run `VACUUM` after each sweep.** Blocks writers for as long as the file is large. Rejected.
