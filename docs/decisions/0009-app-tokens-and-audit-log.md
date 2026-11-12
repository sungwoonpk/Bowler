# 0009. An app can be given a token that works on that app only, and what is done is recorded

Status: accepted

## Context

There was one API key and it could do everything. A company running Bowler had to keep it in its own backend and could
give it to nothing else. That blocks the obvious next step, a page where one of the company's customers manages its own
endpoints and reads its own delivery log. That page runs in a browser, and a browser must never hold a key that can read
every customer's data. Separately, with one key there was no record of who changed what.

## Decision

**Tokens**

- The operator's key makes a **token** for an app (`POST /v1/apps/{app}/tokens`). The token starts with `bwl_` and has 256
  bits of randomness. It is shown **once**, in the response that makes it.
- Only a **SHA-256 hash** is stored, with the first ten characters so that a person can tell tokens apart. A copy of the
  database does not hand out working tokens. A slow password hash would add cost on every request and nothing else, because
  there is nothing short to guess.
- A token acts on **its own app only**: that app's endpoints, messages, deliveries and audit log. Every handler that has an
  app in its path goes through one function, `s.app`, which refuses a token that belongs to another app. A token **cannot make
  apps or tokens, list apps, or read metrics**. Those routes take the operator's key only.
- **Another app looks exactly like one that does not exist**: both are `404`. A token cannot be used to find out which apps
  there are.
- A refused request from a valid token is `403` and is **not** counted as a wrong key, so a valid token is never throttled for
  asking for something it may not have. A wrong, unknown or revoked token is `401` and is throttled like a wrong key.
- **Revoking** takes effect on the next request, because every request looks the token up. Revoking twice changes nothing.
  An app may hold 50 working tokens.

**Audit log**

- Every change an API caller makes is recorded: app and token creation, token revocation, endpoint create, update and
  delete, **reading** and **rotating** a signing secret, `recover`, and `replay`. Each entry says who (the operator, or a token
  id), what, to which resource, when, and the request id that links it to the server log.
- The detail holds **what changed, not the values**: an update lists the names of the fields. Secrets, tokens and payloads
  are never written to it, and a test checks this.
- Sending a message is not in the log. A message is its own record, and a log entry per message would be most of the table.
- Entries are written in order and read in order. A first version sorted by time and then by random id, and two actions in the
  same millisecond came out in the wrong order, so entries carry a sequence number.
- It is read with `GET /v1/apps/{app}/audit-log` (the operator, or that app's token) and `GET /v1/audit-log` (the operator).
- The log is kept for `--audit-retention` (default **365 days**, at least 7 days, `0` for ever), separate from the retention of
  messages, since a record of who did what is worth keeping longer than the messages it is about.

## Costs

- **A full token has the whole authority of its app.** It can read and rotate secrets and create endpoints that point anywhere
  the SSRF rules allow. It is meant for the app's own side. A customer portal would want a layer on top that narrows it.
  *Later added:* a token can be made **read only** (it may look, may not change anything, and may not read a signing secret,
  though reading it is a GET), and can be made to **expire** after up to a year. There is nothing between read and full.
- **The service records roughly when a token was last used**, to within ten minutes. It is written at most that often per token,
  so that using a token does not turn each request into a database write. It is for finding tokens nobody uses, and it is not
  an exact record. The audit log is the exact one for what a token did.
- **The audit entry is written after the action, not in the same transaction.** If the write fails, the action has happened and
  the entry is missing. The caller is told the action worked, the failure is logged, and `bowler_audit_errors_total` counts
  it, so a gap is noticed and not hidden. Making them one transaction would mean passing the entry into every store method.
- **The log records successes, not refusals.** A wrong key is in the request log, and a request that fails is not an action.
- **The operator's key is still all-powerful and appears in the log only as `admin`.** If two people share the key, the log
  cannot tell them apart. That needs named operator credentials, which are not built.
- **Revoking costs one lookup per request**, in the database, on a unique index.

## Alternatives considered

- **Signed tokens such as JWTs, checked without the database.** No lookup, and revocation needs a deny list or short lives,
  which brings the lookup back with more moving parts.
- **A key per app that is also an API key, kept in the environment.** Fixed at start, impossible to rotate while running, and
  it does not scale to many apps.
- **One audit transaction with the change.** More correct, and a much larger change to the store. Chosen against for now,
  with the gap visible as above.
- **Logging every request.** The request log already does that, without a record of what it meant.
