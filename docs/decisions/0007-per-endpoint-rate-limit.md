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
- **Timing is as fine as the poll.** Individual attempts can start up to 250 ms after the moment they became allowed, so
  for a rate such as 3 or 7 per second the starts come in uneven steps (gaps of 500, 250 and 250 ms where 333 was meant).
  The average is not affected. Waking the dispatcher exactly when an allowance returns was considered and **not built**,
  after measuring (below).

## Measured: exact wake-ups are not worth building

With the production poll interval (250 ms) and a backlog waiting, the time from the first attempt to the last of
`rate + 4 × rate` attempts, against the ideal of four seconds after the opening burst:

| Limit | Attempts | Last start after the first | Rate reached |
| ----- | -------- | -------------------------- | ------------ |
| 3 / s | 15       | 4.05 s                     | 99% of the limit |
| 7 / s | 35       | 4.07 s                     | 98% of the limit |
| 20 / s | 100     | 4.08 s                     | 98% of the limit |

A short run at a limit of 1 to 10 per second (five attempts after the burst) gave 98% to 99% as well. The average holds
because the allowance may run ahead of the clock by one second: a poll that is late finds more room and takes more, so the
lateness is made up, not lost. What the 250 ms poll costs is the *spacing* of attempts inside a second, which the burst
allowance already makes uneven by design. A timer that wakes the dispatcher at the exact moment would even out the spacing
and add a second source of wake-ups to the dispatcher loop, which has to be kept correct. That is not worth it for a webhook
receiver. If someone needs smooth spacing, the poll interval is a setting. The measurement is a one-off with a throwaway
test, not part of the suite, because a timing test of several seconds would fail on a slow CI runner.
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
