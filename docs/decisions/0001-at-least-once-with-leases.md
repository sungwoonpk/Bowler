# 0001. Delivery is at least once, and a lease decides who works on a delivery

Status: accepted

## Context

Bowler must not lose an event once it has said "accepted". The process can be killed at any moment, and a worker can
die in the middle of an attempt. A receiver can also time out after it has processed a request, so Bowler cannot always
tell whether an attempt happened.

No system can promise "exactly once" over an unreliable network. The honest choices are "at most once", which loses
events, and "at least once", which can repeat them.

## Decision

- Delivery is **at least once**. A message is stored together with its deliveries in one transaction, before the API
  answers `202`. From then on the only way for it to disappear is for someone to delete the database.
- A worker takes a delivery under a **lease**: a time until which nobody else may take it. If the worker finishes, it
  records the result and the lease ends. If it dies, the lease runs out and another worker takes the delivery.
- The queue is the `deliveries` table. There is no separate broker to keep in step with the database, so there is no
  window in which an event is in one and not the other.
- An attempt is **counted when it is claimed**, not when it ends. A delivery that crashes its worker every time
  therefore still runs out of attempts, instead of being retried for ever.
- A result that arrives after the lease was given away (a slow worker that came back) is **recorded as an attempt but
  does not change the delivery**. The new holder owns it.
- Shutdown stops taking work, lets attempts in progress finish for a drain time, and hands back any it cut off
  **without counting them**, so another worker takes them at once.
- The same `webhook-id` (the message id) is sent on every attempt. A receiver that ignores an id it has seen turns
  at-least-once into effectively-once on its side.
- A claim and a result are both done in a transaction that takes the database write lock first, so two workers can
  never be handed the same delivery. A test starts twelve workers on one queue and checks that.

## Costs

- **A crash or a slow response can cause a repeat.** This is the price of not losing events. Receivers need to
  ignore ids they have already handled. The documentation says so in its first paragraph.
- **A crash delays the work that was in flight** by up to the lease. The default lease is the attempt timeout plus 45
  seconds. It can be shortened, but must stay clearly longer than an attempt, or a running attempt would be given to a
  second worker. The program refuses a lease that is too short.
- Order is not guaranteed across messages. With an endpoint concurrency of one, deliveries go out one at a time in the
  order they became due, which is usually the order they were sent, but a retry can be overtaken by a newer message.

## Alternatives considered

- **An external queue (Redis, Kafka, SQS).** Better throughput at scale, but it adds a second system that must agree
  with the database about what exists. Every place the two can disagree is a place an event can be lost.
- **Exactly-once delivery.** Not possible over a network without the receiver's help, and promising it would be wrong.
- **Acknowledge before storing.** Faster, and loses events on a crash. Rejected.
