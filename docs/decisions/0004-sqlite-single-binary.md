# 0004. One SQLite file and one static binary

Status: accepted

## Context

A webhook service has two kinds of users. A company wants to run it with as little to operate as possible. A team that
grows wants more throughput and more than one machine.

## Decision

- Store everything in **one SQLite file**, through `modernc.org/sqlite`, which is SQLite translated into Go. There is no C
  compiler involved, so Bowler builds with `CGO_ENABLED=0` for Linux, macOS and Windows from any one machine, and ships as a
  single static file of about 16 MB.
- Use **WAL mode**, so reads continue while a write is in progress, and `synchronous=NORMAL`, which is safe with WAL: a
  power cut can lose the last few commits but cannot corrupt the file.
- **Every transaction takes the write lock when it starts.** If it took the lock only at the first write, two workers could
  both read the same due delivery and discover the clash afterwards.
- **Writes inside the process take turns on a mutex.** SQLite allows one writer anyway. Under heavy load, many goroutines
  waiting on the database lock at once ran out of patience and failed with `SQLITE_BUSY`, which a test with twelve workers
  showed. Taking turns in the process is cheaper and steadier. The database lock and busy timeout still protect against a
  second process on the same file.
- The schema changes through **numbered migrations** embedded in the binary and applied at start-up, each in its own
  transaction.
- Times are stored as whole **milliseconds since the epoch**, so they compare and sort as plain integers.
- The queue sits behind the store's methods, so replacing the database means rewriting one package.

## Costs

- **One node.** Several dispatchers in one process share the database, which the tests exercise with twelve workers.
  Several processes on one file on one machine should also work through the database lock, but that is not tested.
  Nothing works across machines. Running more than one machine needs a networked database, the first item on the roadmap.
- SQLite has one writer, so write throughput is bounded by the speed of one disk and one core.
- The file is the only copy of the data. Backups need `sqlite3 .backup` or a copy taken with the service stopped.
- Signing secrets are stored as they are, see the threat model.

## Alternatives considered

- **Postgres from the start.** The right production database and the better answer for many nodes. It would also mean that
  trying Bowler needs a database server. The store is shaped so that Postgres can be added behind the same methods.
- **SQLite through the C library (`mattn/go-sqlite3`).** Faster in some cases, and it needs a C toolchain, which breaks
  one-command cross compilation and the small static image.
- **An embedded key-value store.** Fewer features than SQL, and the queue's per-endpoint fairness query is a single SQL
  statement with a window function.
