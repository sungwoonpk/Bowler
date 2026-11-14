# 0003. Customer URLs are checked when they are registered and again when a connection is made

Status: accepted

## Context

Bowler makes HTTP requests to addresses that its users choose. If it will call any address, a user can register
`http://169.254.169.254/latest/meta-data/` and have Bowler fetch the cloud credentials of the machine it runs on, or point
it at a database or an admin page on the private network. This is called server-side request forgery.

Checking the URL when it is registered is not enough. The URL names a host, and a host name can resolve to a public
address on the day it is registered and to a private one later. An attacker who controls the DNS for their own domain can
do exactly that, and can also answer differently on each lookup.

## Decision

- **At registration**, the URL must be `https`, have no credentials, and must not name `localhost` or a private or reserved
  address written out in the URL. This gives the person registering a clear error.
- **At connection time**, a `Control` function on the dialer receives the IP address the name resolved to, just before
  each connection attempt, and refuses private, loopback, link-local (which includes the cloud metadata address),
  carrier-grade NAT, multicast, reserved and documentation ranges, for IPv4 and IPv6. An IPv4 address written as IPv6
  (`::ffff:10.0.0.1`) is unwrapped first. This is the check that counts, because it sees the address that is really used.
- **Redirects are not followed.** A receiver on a public address could otherwise answer `302` to `http://169.254.169.254/`
  and have Bowler walk there. A redirect is reported as a failure instead.
- **Proxies from the environment are ignored.** With `HTTP_PROXY` set, the connection would go to the proxy, the address
  check would see only the proxy, and the real destination would never be checked.
- An endpoint that fails the connection check is **switched off** rather than retried, because retrying cannot help.
- A policy flag, off by default, allows private addresses and plain `http` for development, demos and tests, where the
  receiver runs on the same machine. The program logs a warning when it is on.

## Costs

- Receivers that really are on a private network (an internal system) could at first be reached only with the global flag.
  [0012](0012-operator-allow-list-for-private-receivers.md) lets the operator allow named private ranges instead.
- A host name that resolves to both a public and a private address is refused on the attempt that picks the private one,
  which makes the failure intermittent.
- IP ranges are a list in the code. A new reserved range has to be added by hand.

## Alternatives considered

- **Validate the resolved address at registration and store it.** Breaks when the receiver's address legitimately
  changes, and still leaves a gap if DNS answers differently later.
- **Resolve the name myself and dial the address.** Works, but gives up the standard dialer's handling of multiple
  addresses and fallback. The `Control` hook gets the same protection with less code to get wrong.
- **Run deliveries from a network segment that cannot reach anything private.** The strongest answer and the right one in
  production. It is infrastructure, so it complements this check and does not replace it.
