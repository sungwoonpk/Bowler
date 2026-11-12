# 0005. One receiver cannot hold up the rest, and a receiver that is fixed can catch up

Status: accepted

## Context

Bowler sends to servers it does not control. Some will be slow, some will be down, and one will occasionally say "this
endpoint is gone". A shared queue makes them affect each other: a slow receiver can fill every worker so that a healthy one
waits, and an event that arrives while an endpoint is off can be lost to that customer without anyone noticing.

## Decision

**Isolation**

- Every endpoint has a **concurrency** (default 1, at most 20): how many of its deliveries may be in flight at once.
- Choosing work for the workers is one SQL query that **ranks the due deliveries within each endpoint** and takes at most
  as many from an endpoint as it has free slots. A thousand deliveries waiting for one slow endpoint can occupy one worker,
  not all of them, and the rest go to everyone else. A test queues two hundred deliveries behind a stuck endpoint and checks
  that a healthy endpoint is not delayed.
- Every attempt has a **timeout**, the reply is read only up to a bound, and a receiver that streams for ever cannot hold a
  worker.
- Retries use **jitter**, so many deliveries that failed together do not all come back at the same moment.

**What a failure means**

- A `2xx` is success. A `410 Gone` is a permanent no. Everything else is retried on the schedule, including `4xx`, because a
  receiver that returns `401` or `404` is usually misconfigured and fixing it is the common remedy. A `429` or `503` with
  `Retry-After` is honoured, up to a day.
- A permanent no **switches the endpoint off** and finishes the deliveries that were waiting for it as failed. They are
  not left pending for ever.
- Switching an endpoint off **by hand is a pause**. Its deliveries wait and resume when it is switched back on.

**Recovery**

- `recover` is the one call to make after a receiver is fixed. It switches the endpoint on, replays deliveries that failed
  since a given time, **and creates deliveries for messages that arrived while the endpoint was off**, which new messages
  skip. Without that last part, a customer who fixes their server would never get the events from the time it was down.
- It leaves out messages from before the endpoint existed and types the endpoint does not subscribe to, and never makes a
  second delivery for a message that already has one.

## Costs

- A concurrency of one makes a slow receiver process its backlog slowly. That is the intended trade: it is gentle on a
  receiver that is struggling. A receiver that can take more can raise it.
- Retrying `4xx` means a receiver that rejects every request is retried for the whole schedule, about a day. The history
  shows why each attempt failed, so the customer can see it.
- Recovery after a long outage can send a burst. Concurrency and the receiver's own limits bound it, and nothing paces it
  beyond that yet.

## Alternatives considered

- **A queue per endpoint.** Isolates well and needs a worker per queue. One query with a ranking gets the same fairness
  from one shared queue.
- **Give up on an endpoint after N failures in a row.** Common, and it hides real outages from the customer. A switch-off
  that follows the receiver's own `410`, plus a clear history, is easier to explain.
