# 0007. An endpoint can be limited to a number of attempts per second

Status: accepted

## Context

Concurrency (0005) limits how many attempts are in flight, and says nothing about how fast they start. A receiver that
answers in 5 ms with a concurrency of 20 can still be sent 4,000 requests a second. That is how a customer's server is
flooded by a burst of events, or by `recover` replaying a long outage. 0005 listed this as a cost and left it open.

## Decision

- An endpoint has `rate_limit_per_second`, from 1 to 1000, or 0 for no limit. It can be set when the endpoint is created
  and changed later.
- It is enforced **when work is chosen**, in the same transaction and the same query as concurrency. A claim takes from an
  endpoint no more than its free slots **and** its remaining allowance.
- The allowance is the **generic cell rate algorithm**. Each endpoint stores one number, `rate_tat`: the time up to
  which its allowance has already been handed out. Each delivery taken moves it forward by one interval (1000 ms divided by
  the rate), and the endpoint may run ahead of the clock by one second. After a quiet time, up to one second's worth (the
  rate itself) can go at once, and after that the rate holds. A token bucket needs two numbers per endpoint. This needs one.
- The number is **in the database**, so a restart does not hand out a fresh allowance, and several instances sharing the
  file share one allowance. A test restarts the store and checks.
- Deliveries held back stay pending and are found on the next poll (at most 250 ms later). Nothing is rescheduled and
  nothing is marked failed, so a limit never uses up the retry schedule.
- A limit applies to everything: new messages, retries, `recover`, and manual replays.

## Costs

- **A burst of one second's worth is allowed after a quiet time.** A receiver that cannot take 10 requests at the same
  instant should use a concurrency of 1 together with the rate. The allowance is in the documentation, so it is not a
  surprise.
- **Timing is as fine as the poll.** A limit of 1 per second is kept on average, and individual attempts can start up to
  250 ms after the moment they became allowed. Waking the dispatcher exactly when an allowance returns would be more
  precise and is not built.
- **The interval is a whole number of milliseconds**, so a rate that does not divide 1000 (for example 3) is slightly
  fast, about 0.1%.
- The limit is per endpoint and says nothing about the app as a whole. An app with many endpoints is limited by the sum.
- A claim does one more write per limited endpoint that it takes work from. The write happens in the claim's transaction,
  so it costs a statement and not a commit.

## Alternatives considered

- **A token bucket in memory.** Simple, and lost on restart, and not shared by several instances. A restart would let a
  burst through at the worst moment, just after an outage.
- **Rate limits as a delay on each delivery** (set `next_attempt_at` into the future). It spreads the work, and it changes
  the history of every delivery and interacts with the retry schedule. Choosing at claim time leaves the data alone.
- **A limit per app.** It does not protect a single customer's receiver, which is the thing being protected.
