# Development

How Bowler is built and checked. How to contribute is in [CONTRIBUTING.md](../CONTRIBUTING.md).

```bash
make check       # format, vet, staticcheck, errcheck, gosec, race tests with coverage, vulnerabilities
```

- **257 tests** run with the race detector, and the libraries have **92% coverage**. CI fails under 85%.
- **A test that kills the real program.** It starts `bowler serve` as a separate process, sends twenty messages to a receiver that holds its requests open, waits until deliveries are in flight, kills the process with `kill -9`, starts it again on the same database, and checks that every message reaches the customer.
- **Concurrency.** Twelve workers share one queue and each delivery is handed out exactly once. Three dispatchers share a queue and nothing is sent twice. A slow endpoint with two hundred deliveries behind it does not delay a healthy one.
- **Failure handling.** Every status class, timeouts, refused connections, DNS failures, bad certificates, redirects, a reply that never ends, a sender that panics, a lease that is lost, shutdown mid-attempt.
- **Security.** Every kind of private address, a name that resolves to loopback, `HTTP_PROXY` ignored, the API key never in the log, one app unable to read another's data. One test walks every route the server registers and checks that a token of one app cannot reach another, and that the operator-only routes refuse tokens.
- **The documentation is tested.** The signing example is compiled and run. The OpenAPI file is compared with the routes the server registers, and with the server's behaviour for tokens, route by route.
- **The tests were tested.** Important ones were checked by breaking the code or the spec on purpose and confirming the test fails. A test that passes the first time proves little.
- **CI** runs all of that, plus a scan for known vulnerabilities, CodeQL, builds for Linux, macOS and Windows, the demo with its output checked, and a container that must become healthy, run as an unprivileged user and stop cleanly.
