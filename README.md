# Bowler

[![CI](https://github.com/sungwoonpk/Bowler/actions/workflows/ci.yml/badge.svg)](https://github.com/sungwoonpk/Bowler/actions/workflows/ci.yml)
[![CodeQL](https://github.com/sungwoonpk/Bowler/actions/workflows/codeql.yml/badge.svg)](https://github.com/sungwoonpk/Bowler/actions/workflows/codeql.yml)

A webhook delivery service. Your product says "this happened", and Bowler makes sure each of your customers' servers hears about it. It signs the request, retries failures, and records what happened on every attempt.

Written in Go. One static file, one SQLite file, no database server and no C compiler.

![Bowler demo](docs/images/demo.gif)

## Why

Almost every product with an API ends up sending webhooks, and the sending side is usually built too quickly.

| What goes wrong                                         | What Bowler does                                                                    |
| ------------------------------------------------------- | ----------------------------------------------------------------------------------- |
| A customer's server is down and the event is lost       | The event is stored before you are told "accepted", and retried for about a day     |
| One slow customer holds up everyone else                | Every endpoint has its own concurrency cap and rate limit                           |
| The same event arrives twice                            | Every attempt carries the same `webhook-id`, so a receiver can ignore a repeat      |
| "Is this really from you?"                              | Signed as the [Standard Webhooks](https://www.standardwebhooks.com) spec says       |
| "You never sent it"                                     | A history of every attempt: when, the status, how long, what the receiver answered  |
| A user registers `http://169.254.169.254/` as a URL     | Refused when registered and again at connection time                                |
| The sending process is killed in the middle of a send   | Nothing is lost. Another worker takes the work when the lease runs out              |

## Features

- **Reliable.** At least once, with retries over about a day, jitter, and `Retry-After`.
- **Fair.** Per-endpoint concurrency and rate limit, so one stuck receiver cannot starve the rest.
- **Recoverable.** Replay one delivery, or `recover` an endpoint and get what it missed while it was off.
- **Signed.** Rotating a secret sends two signatures for a grace period. A dependency-free `signature` package verifies in Go.
- **Safe to point at strangers' URLs.** https only, no private or metadata addresses, no redirects, checked again when connecting.
- **Access control.** An app can get a token that works on that app only, can be read only, and can expire. Every change is in an audit log.
- **Private data.** Signing secrets can be encrypted in the database. Old messages are deleted after a retention period.
- **Operable.** JSON logs, Prometheus metrics, health checks, graceful shutdown, an OpenAPI file kept true by a test.

## Run it

Needs Go 1.26 or newer. No accounts, no keys, no network, no cost.

```bash
git clone https://github.com/sungwoonpk/Bowler.git
cd Bowler
make demo        # five pretend customers that fail in different ways, in about fifteen seconds
```

```bash
export BOWLER_API_KEY=$(openssl rand -hex 24)
go run ./cmd/bowler serve        # listens on 127.0.0.1:8080
```

Or with Docker: `docker compose up --build`. Settings are in [docs/setup.md](docs/setup.md).

## Using it

| To do this                        | Do this                                                      |
| --------------------------------- | ------------------------------------------------------------ |
| Add one of your customers         | `POST /v1/apps`                                              |
| Add a URL that should get events  | `POST /v1/apps/{app}/endpoints` (returns the signing secret) |
| Send an event                     | `POST /v1/apps/{app}/messages`, with an `Idempotency-Key`    |
| See what happened to it           | `GET /v1/apps/{app}/messages/{message}/deliveries`           |
| Catch an endpoint up after a fix  | `POST /v1/apps/{app}/endpoints/{endpoint}/recover`           |
| Give a customer's page access     | `POST /v1/apps/{app}/tokens`                                 |
| See who changed what              | `GET /v1/apps/{app}/audit-log`                               |

Examples with `curl` are in [docs/api.md](docs/api.md), and every route is in [docs/openapi.json](docs/openapi.json). If you are on the receiving end of Bowler's webhooks, read [docs/receiving-webhooks.md](docs/receiving-webhooks.md).

## Checks

```bash
make check       # format, vet, staticcheck, errcheck, gosec, race tests with coverage, vulnerabilities
```

CI runs the same, plus CodeQL, builds for Linux, macOS and Windows, the demo, and a container that must become healthy.

## Docs

| | |
| --- | --- |
| [Setup](docs/setup.md) | Running it, every setting, the encryption key, retention |
| [Using the API](docs/api.md) | Examples, tokens, recovery |
| [Receiving webhooks](docs/receiving-webhooks.md) | Headers, verifying a signature, what to answer |
| [Features](docs/features.md) | What it does, in detail |
| [Architecture](docs/architecture.md) | How a delivery flows, and the code layout |
| [Decisions](docs/decisions/README.md) | Why it is built this way, and what each choice costs |
| [Performance](docs/performance.md) | Numbers and what measuring found |
| [Guarantees, limits and roadmap](docs/limits.md) | What it promises, what it does not do, what is next |
| [Development](docs/development.md) | Tests and how they were checked |
| [Threat model](docs/threat-model.md), [Security](SECURITY.md) | What is protected and what is not, and how to report a problem |

## Status

Built milestone by milestone, each with tests and a decision record. It has had no independent security review and runs on one node. The limits are listed plainly in [docs/limits.md](docs/limits.md).
