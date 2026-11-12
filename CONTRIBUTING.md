# Contributing

## Set up

Needs Go 1.26 or newer. There is nothing else to install: no C compiler, no database server. Use a recent patch release: the
standard library is part of what ships, and `make vuln` fails on the flaws that older patches have.

```sh
git clone https://github.com/sungwoonpk/Bowler.git
cd Bowler
make check      # format, vet, staticcheck, race tests with coverage, vulnerability scan
```

`make check` needs four tools, which `go install` fetches:

```sh
go install honnef.co/go/tools/cmd/staticcheck@latest
go install github.com/kisielk/errcheck@latest
go install github.com/securego/gosec/v2/cmd/gosec@latest
go install golang.org/x/vuln/cmd/govulncheck@latest
```

| Command        | What it does                                                                |
| -------------- | --------------------------------------------------------------------------- |
| `make test`    | All tests                                                                   |
| `make race`    | All tests with the race detector, which is how CI runs them                 |
| `make cover`   | Coverage of the libraries                                                   |
| `make lint`    | gofmt, vet and staticcheck                                                  |
| `make demo`    | Build and run the demo                                                      |
| `make build`   | Build `bin/bowler`                                                          |
| `make docker`  | Build the container image                                                   |

## How the code is laid out

- `signature/` and `retry/` are public packages with no dependencies. A receiver can import `signature` to verify webhooks.
- `internal/store` is the only code that touches the database. It owns the queue: claiming under a lease, recording results,
  fairness between endpoints, replay and recovery.
- `internal/dispatcher` runs the queue: it takes due deliveries, hands them to a sender, records the result, and shuts down in
  order.
- `internal/deliver` makes one attempt. It knows nothing about retries, it only reports a verdict.
- `internal/ssrf` decides which addresses may be called.
- `internal/api` is the HTTP interface. `docs/openapi.json` describes it, and tests fail if the two differ.
- `cmd/bowler` is the program: `serve`, `demo`, `healthcheck`, `version`.

## Making a change

1. **Write the test first** when you fix a bug, and watch it fail for the reason you expect.
2. **A test that passes the first time proves little.** Break the code on purpose and check that the test notices.
3. Add or change a route in `internal/api` **and** in `docs/openapi.json`. A test lists the routes the server has and compares
   them with the file.
4. Anything that touches the queue needs a test that runs with `-race`. The queue is shared by many goroutines, and a mistake
   there loses or repeats events without any error.
5. If you change a design decision, write or amend a record in `docs/decisions`. Say what it costs, not only why it is good.
6. Keep the README true. Check each claim you touch against the code.
7. Commit small, with a message that says what changed and why. Everything in the repository is written in English.

## Style

`gofmt` and `staticcheck` decide formatting and most style questions. Beyond that: write a comment for the reason behind
something and not for what it does, return errors with enough context to find them, never log a secret, and fail closed when a
security decision is unclear.

## Security

Never put a real key, a real URL of a customer, or real customer data in the repository, in a test or in an issue. See
[SECURITY.md](SECURITY.md) for how to report a vulnerability.
