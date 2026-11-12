# 0006. Results are written in batches by one writer

Status: accepted

## Context

SQLite has one writer. Every attempt ends with a write (the attempt record and the new state of the delivery), and in the
first version each of those was its own transaction. Measuring showed the database, not the network, was the limit:
about 4,000 to 5,500 deliveries a second with a sender that answered at once. Each transaction pays for a commit, and a commit is
most of the cost of a small write.

## Decision

- Workers do not write their own results. They hand the result to **one writer goroutine** and wait until it is on disk.
- The writer collects results into a **batch** and writes the batch in one transaction. A batch is written when it holds
  64 results, or 2 ms after its first result arrived, whichever comes first. A result that arrives alone waits at most
  2 ms.
- Inside the transaction **each result has its own savepoint**. If one result cannot be written (a failed constraint, for
  example), only that one is rolled back, and the rest of the batch is committed. Each worker is told how its own result
  went, so a lost lease is still reported to the right worker.
- A worker **waits for the answer** before it gives up its slot. A slow disk therefore slows the intake of new work and
  does not let results pile up in memory.
- On shutdown the writer starts first and stops last: it is closed only after every attempt has reported, so no result
  is lost on the way out.
- `bowler_results_total` and `bowler_result_batches_total` show how well batching works. Their ratio is the average batch.

## Costs

- A result is written up to 2 ms later than it could have been. It is invisible next to an HTTP request, and it is
  what lets results that arrive together share a commit.
- A failure of the transaction itself (a full disk) fails every result in the batch. Each worker logs it and the
  deliveries are picked up again when their leases run out, so nothing is lost, but the cost of one failure is larger.
- One more goroutine and one more place where order matters at shutdown. A test checks that every result is written
  exactly once, and that results arriving together share a transaction.

## Measured

On one laptop, same session, run back to back: the queue alone went from about 4,000 to 4,900 up to about 6,300 to 6,900
deliveries a second, and real HTTP from about 5,000 to 5,800 up to about 7,500 to 7,900. Measured again later with the
longer benchmark: about 6,800 to 7,400 and 5,700 to 6,300. The machine's own speed varies from hour to hour, so only the
comparison inside one session means much.

## Alternatives considered

- **A transaction per result, as before.** Simple and slow.
- **Lower the durability** (`synchronous = OFF`) **to make commits cheap.** A crash or power cut could then lose results the
  service had already reported as done, which breaks the promise in 0001. Rejected.
- **Let workers batch among themselves** (the first to arrive writes for the others). It has no extra goroutine, and it is
  harder to reason about and to shut down in order.
- **Move to Postgres.** On the roadmap, for running more than one node. It is not needed to get past this limit.
