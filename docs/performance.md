# Performance

Measured with `go test ./internal/dispatcher -bench . -benchtime 2000x`, on an Apple M3 Pro, one process, one SQLite file on the local disk. Each message goes to ten endpoints. The numbers cover the whole path: storing the message, taking the work, sending, recording the result.

| What is measured                                             | Result                                              |
| ------------------------------------------------------------ | --------------------------------------------------- |
| The queue and database alone (a sender that answers at once) | about 6,800 to 7,400 deliveries a second            |
| Real signed HTTP requests over the loopback interface        | about 5,700 to 6,300 deliveries a second, four runs |
| Cost of choosing work, 1,000 to 50,000 deliveries waiting    | about 0.2 ms, the same at every size                |

These are from one laptop on loopback. They show where the limits are, and are not a promise about your hardware or your customers' servers. The machine varies from hour to hour, so only a comparison made in one sitting means much.

**What measuring found.** Each of these only shows when the service is busy, and each now has a test or a record behind it.

- **A backlog made the queue slower.** Choosing work ranked every due delivery, so one claim took 2 ms with 1,000 waiting and 190 ms with 50,000, and a service that fell behind fell further behind. It now reads one endpoint at a time through partial indexes, and a test fails if the cost grows with the backlog.
- **Half of all requests opened a new connection.** The HTTP client kept four idle connections per host while an endpoint may run twenty deliveries at once. Under load that used up local ports, and throughput swung between 27 and 4,200 deliveries a second. New connections fell from about 500 to about 19 per thousand deliveries.
- **Every result was its own transaction**, and a commit is most of the cost of a small write. A writer now records results that arrive together in one transaction, each with its own savepoint so one bad result cannot spoil the rest. Run back to back in one sitting, that took the queue from about 4,000 to 4,900 up to about 6,300 to 6,900 deliveries a second, and real HTTP from about 5,000 to 5,800 up to about 7,500 to 7,900. [0006](decisions/0006-batched-result-writes.md)

The ceiling is still SQLite's single writer, only higher. Moving to a networked database is on the roadmap.
