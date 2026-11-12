# Threat model

What Bowler protects, who it protects it from, what it does about each threat, and what it does **not** do. It is written for
someone deciding whether to trust the service, so the gaps are listed as plainly as the defenses.

## What is being protected

| Asset                                | Why it matters                                                                          |
| ------------------------------------ | --------------------------------------------------------------------------------------- |
| The machine Bowler runs on           | Bowler makes requests to addresses its users choose, so it can be turned into a weapon  |
| The API key                          | It can read and change everything                                                       |
| App tokens                           | Each has the whole authority of one app, and a leaked one lets someone act as that app  |
| The audit log                        | It is the record of who did what, so it must be complete and must not hold secrets      |
| Endpoint signing secrets             | Anyone who has one can forge webhooks to that endpoint                                  |
| Message payloads                     | They often carry customer data: names, amounts, addresses                               |
| Delivery history                     | It shows what each receiver was sent and what it answered                               |
| Receivers (the customers' servers)   | A bad sender can flood them, or trick them into accepting something forged              |

## Who might attack, and from where

1. **A user who registers a hostile URL.** Pointing it at the cloud metadata service, the loopback interface, or a machine on the
   private network, to read what Bowler can reach.
2. **A hostile receiver.** A server that hangs, streams for ever, sends enormous replies, redirects somewhere private, or tries to
   make Bowler retry without end.
3. **Someone who wants to forge or replay a webhook** to a customer's server.
4. **Someone who guesses the API key**, or who has read access to logs, backups or the process list.
5. **A caller who sends bad input**: huge bodies, odd JSON, odd headers, ids shaped to confuse a query.
6. **A user of one app who tries to reach another app's data.**
7. **A crash, or a power cut,** which is not an attack and is the most likely thing to go wrong.

## Threats and what is done about them

| Threat                                                      | Defense                                                                                                                                         | Checked by                          |
| ----------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------- |
| A URL aimed at private or metadata addresses (SSRF)         | https only, and no private, loopback, link-local, CGNAT or reserved ranges, checked when registered and again on the address actually dialled   | `ssrf`, `deliver`, `api` tests      |
| A name that resolves to a public address, then a private one | The check runs on the resolved address at connection time, so a later answer from DNS is checked too                                           | `ssrf` tests (a real dial)          |
| A receiver that answers `302` to an internal address        | Redirects are never followed, they count as a failed attempt                                                                                    | `deliver` tests                     |
| A proxy in the environment hiding the real destination      | The HTTP client ignores `HTTP_PROXY` and friends                                                                                                | `deliver` tests                     |
| A receiver that never answers                               | A timeout on every attempt, and a lease that outlasts it by a checked margin                                                                    | `deliver`, `dispatcher` tests       |
| A receiver that streams for ever                            | The reply is read only up to a bound, then dropped                                                                                              | `deliver` tests                     |
| A slow or dead receiver delaying everyone else              | A per-endpoint concurrency cap, and a queue query that ranks work per endpoint so a backlog cannot fill the workers                             | `store`, `dispatcher` tests         |
| Many failures all retrying at the same moment               | Jitter on every retry wait                                                                                                                      | `retry` tests                       |
| A receiver asking for an absurd wait                        | `Retry-After` is honoured only up to one day                                                                                                    | `retry` tests                       |
| A forged webhook                                            | HMAC-SHA-256 signature over id, time and body, with one secret per endpoint                                                                     | `signature`, `api` tests            |
| A captured webhook sent again later                         | A timestamp in the signature that the receiver is told to check, and an id that stays the same so repeats are recognizable                      | `signature` tests                   |
| One app's token used on another app                         | Every route with an app in its path goes through one check, and another app answers `404`, the same as one that does not exist, so tokens cannot be used to find apps | `api` tests, which walk every registered route |
| A token used where only the operator may                    | Making apps and tokens, listing apps, and metrics take the operator's key only, and the spec says so route by route, checked against the server | `api` tests                         |
| A copy of the database handing out tokens                   | Only a SHA-256 hash of each token is stored, and the token is shown once                                                                        | `store`, `api` tests                |
| A leaked token that must stop at once                       | Revoking takes effect on the next request, since every request looks the token up                                                               | `api` tests                         |
| Nobody knowing who changed an endpoint or read a secret     | An audit log of changes, secret reads and rotations, naming the operator or the token, with no secrets or values in it                           | `store`, `api` tests                |
| A guessed API key                                           | At least 24 characters are required, comparison takes constant time, and repeated wrong keys from one address are slowed down                   | `api`, `cmd` tests                  |
| A right key locked out by someone else's guessing           | The key is checked first, and the slow-down applies only to a wrong one                                                                         | `api` tests                         |
| The API key showing in the process list                     | It can only be set through the environment, never as a flag                                                                                     | `cmd` tests                         |
| A secret in the logs                                        | Request logs carry method, path, status and time only. Query strings, headers and bodies are left out                                           | `cmd` process test                  |
| Bad input                                                   | Strict JSON (unknown fields refused), size limits, id formats checked before any query, names and event types matched against patterns          | `api` tests                         |
| One app reading another's data                              | Every read and write takes the app id and the resource id together, so a wrong pairing finds nothing                                            | `store`, `api` tests                |
| Internal errors leaking details                             | They return a fixed message and go to the log, with the request id to connect the two                                                           | `api` tests                         |
| A lost event on a crash                                     | The message and its deliveries are stored in one transaction before `202`, and work is taken under a lease                                      | `cmd` test with `kill -9`           |
| Two workers sending the same delivery                       | The claim happens in a transaction that takes the write lock, and a worker that lost its lease cannot overwrite the new holder's result         | `store`, `dispatcher` tests         |
| A running attempt given to a second worker                  | The program refuses a lease that is not clearly longer than the attempt timeout                                                                 | `cmd`, `dispatcher` tests           |
| A customer's server flooded by a burst or a replay          | An optional per-endpoint rate limit, enforced when work is chosen and kept in the database, so a restart does not reset it                      | `store`, `dispatcher` tests         |
| Customer data kept for ever                                 | Finished messages, with their deliveries and attempts, are deleted after a retention period, never while any work on them is left               | `store`, `retention` tests          |
| An endpoint marked gone being retried for ever              | A `410` switches the endpoint off and finishes what was waiting, and `recover` brings it back                                                   | `store`, `api`, demo                |
| Events lost to a customer during an outage                  | `recover` also creates deliveries for messages that arrived while the endpoint was off                                                          | `store`, `api` tests                |

## What is not covered

These are real gaps. Read them before relying on the service.

- **Signing secrets are stored in the clear** in the SQLite file. They have to be readable to sign with. Anyone who can read the file
  can forge webhooks to every endpoint. Keep the file on an encrypted volume that only the service can read. Encrypting the secrets
  with a key kept outside the file is on the roadmap.
- **Payloads and the first kilobyte of each reply are stored** in the file too, and may contain personal data. They are deleted
  after the retention period (30 days unless changed), but until then they sit in the clear, and a backup taken earlier keeps
  them. Deleted rows do not shrink the file, and the space is reused until a `VACUUM`, so deleted text may still be in the file
  on disk.
- **The API key is all-powerful.** There are no roles for operators, and the audit log shows the key only as `admin`, so two people
  sharing it cannot be told apart. Put Bowler behind your own backend, and do not hand the key to anything you do not trust fully.
- **A token has the whole authority of its app.** There are no read-only tokens or narrower scopes. A token can read and rotate
  signing secrets and create endpoints, so it is for the app's own side. It does not expire until revoked, and the service does
  not record when it was last used.
- **The audit entry is written after the action.** If writing it fails, the action has happened and the entry is missing. The
  failure is logged and counted in `bowler_audit_errors_total`. Refused requests are not in the audit log, only in the request log.
  An operator with access to the database file can still edit it, since it is not signed or sent anywhere else.
- **No TLS.** The API speaks plain HTTP. Run it behind a reverse proxy or load balancer that terminates TLS.
- **No rate limit on the API** except on wrong keys. A caller with the key can send as fast as the machine allows. The
  per-endpoint rate limit protects the *receivers* from Bowler, not Bowler from its callers.
- **Receivers on a private network cannot be reached** unless the global development flag is turned on, which also removes the SSRF
  protection for everyone. A per-endpoint allow list is not built yet.
- **One node.** There is no replication. If the disk is lost, the data is lost, so back the file up.
- **At-least-once means repeats are possible.** A receiver that does not ignore a `webhook-id` it has seen can process an event twice.
- **Delivery order is not guaranteed.**
- **Demo grade.** It has had no independent security review.

## How to report a problem

See [SECURITY.md](../SECURITY.md).
