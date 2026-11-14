# 0011. The audit entry is written in the same transaction as the change

Status: accepted. It replaces the "written after the action" part of [0009](0009-app-tokens-and-audit-log.md).

## Context

The audit log was written by the API after the store had made the change, as a second write. If that second write failed, the
change had happened and the entry did not exist. The failure was logged and counted in `bowler_audit_errors_total`, so the gap
was not hidden, but it was still a gap in the one record whose job is to have none. Reading a signing secret had the same
problem: the secret could be handed out before anyone knew the entry was missing.

## Decision

- **A change and its entry are one transaction.** Every store method that changes something takes a `store.Audited(entry)`
  option and writes the entry inside the transaction that makes the change. Either both exist or neither does.
- **The store completes the entry.** The caller says who, what action and which request. The store adds the id and the time,
  and the app and the resource it has just changed wherever the caller left them empty. That is the only way to name a
  resource that the change itself creates, such as a new endpoint, and it keeps a caller from naming the wrong one.
- **A change that changes nothing writes nothing.** Revoking a token that is already revoked leaves the log alone. Before, the
  API decided this by reading the token first, so two revokes at the same moment could leave two entries. Now the transaction
  decides, and a test with eight callers at once finds one entry.
- **A failed entry refuses the request.** The caller gets a `500` saying the change was not made, and nothing changed, so
  repeating it is safe. `bowler_audit_errors_total` now counts these refusals, which makes it the alarm for an audit log that
  has stopped working.
- **Reading a secret writes its entry first.** A read changes nothing, so there is no transaction to share. The entry is
  written before the secret is returned, and if that fails the secret is not returned. No record, no secret.
- **Work that is not audited is not held up.** Sending a message, delivering it and reading a list never ask for an entry, so a
  broken audit log stops the changes people make and not the webhooks.

## Costs

- **A broken audit log now stops changes.** That is the point of the decision, and also its price. If the table cannot be
  written, an operator cannot create an endpoint until it can. The metric and the log line say why.
- **Nine store methods grew an optional argument**, and `RotateSecret` also takes the current time, which the entry needs.
- **One more write per change.** Most of these changes already ran in a transaction, so the entry is one insert in it. Creating
  an app or an endpoint was a single statement and now runs in a transaction too, taking its turn with the other writers. These
  are changes made by people, not the delivery path, so the cost does not reach the delivery rate.

## Alternatives considered

- **Keep the second write and keep counting failures.** Simple, and the gap stays possible. Rejected now that the store is
  small enough to carry the entry.
- **A database trigger writing the entry.** Atomic with no change to callers, but a trigger does not know who did it or which
  request it was, so the entry would lose the part that matters.
- **Pass the entry in the context.** Less typing, and a store method would write an entry that nobody can see being asked for.
  An explicit option shows at the call that it is audited.
- **Write the entry first for every change.** It would leave an entry for a change that then failed, which says the opposite
  of the truth.
