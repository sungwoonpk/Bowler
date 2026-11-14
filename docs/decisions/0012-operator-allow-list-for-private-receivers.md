# 0012. The operator can allow named private ranges, and a customer cannot

Status: accepted. It replaces the "a per-endpoint allow list would be better" cost of [0003](0003-ssrf-at-connection-time.md).

## Context

The SSRF check refuses every private address, and the only way to reach a receiver on a private network was a global
development flag, `--allow-private`. That flag removes the protection for every endpoint of every customer at once, so a
company whose own systems sit on `10.20.0.0/16` had to choose between not delivering to them and giving every customer the
power to aim Bowler at the whole internal network, the cloud metadata address included.

The roadmap called for a per-endpoint allow list. Building it showed two problems.

- **Who may set it.** A field on an endpoint is something an app's own side can write. If a customer can say that their
  endpoint may reach `169.254.169.254`, the protection is gone. It would have to be a field only the operator's key can write,
  on a route that customers also use, with a rule for each of create and update.
- **Connection reuse.** The HTTP client keeps connections open and reuses one for the same host and port. The address is
  checked only when a connection is made. A connection opened for an endpoint that is allowed to reach a private address can then
  be reused for another endpoint, with another allow list, that names the same host. Closing that gap means one connection pool
  per allow list, which is a lot of machinery in the one place where a mistake is a security hole.

## Decision

- **One list, set by the operator at start-up.** `--allow-cidr` (or `BOWLER_ALLOW_CIDRS`) names private ranges, such as
  `10.20.0.0/16`. A destination in one of them is allowed, and every other private address stays blocked. It applies to the
  URL check at registration and to the check on the address actually dialled, so a name that resolves into the range works
  and a name that resolves anywhere else private does not.
- **The list can only be made smaller than the danger.** The program refuses to start, and says why, if a range
  - is not written as a network (`10.20.0.5/16` has bits set after the network part, and is probably a typo),
  - is not inside one of the private ranges people keep receivers in (10/8, 100.64/10, 127/8, 169.254/16, 172.16/12,
    192.168/16, ::1, fc00::/7, fe80::/10), which rules out `0.0.0.0/0`, every public range and a range that reaches into one,
  - includes a cloud metadata address (169.254.169.254, fd00:ec2::254, 100.100.100.200 or 192.0.0.192), however wide the range, or
  - is in the NAT64 or 6to4 ranges, which carry an IPv4 address inside them and could name a metadata address another way.

  One bad entry refuses the whole list, so a typo cannot quietly leave a range out. At most 64 ranges are accepted.
- **The start-up log says which ranges are open.** `--allow-private` still allows everything and still warns. Giving both is
  allowed, and logs that the list does nothing.
- **No localhost by name.** A URL that says `localhost` is still refused when it is registered, even with `127.0.0.0/8` in the
  list, because the registration check does not resolve names. The operator writes `127.0.0.1`.

## Costs

- **Every customer can reach an allowed range.** The list is for the operator's own network, where that is acceptable, and not
  for a range that holds things customers must not reach. If one app needs a private receiver and others must not have it,
  this is the wrong tool, and the answer is a separate Bowler for that app.
- **It is not per endpoint,** which was the roadmap's wording. The operator cannot allow one endpoint to reach a range and
  refuse another endpoint the same, for the two reasons above.
- **A change needs a restart.** The list is read once, like the other settings.
- **Allowing `127.0.0.0/8` lets customers aim Bowler at its own API port.** A webhook is a signed POST without an
  `Authorization` header, so it cannot act as the operator, and it still tells a customer something about the machine. Allow
  loopback only where that is acceptable.

## Alternatives considered

- **A field on the endpoint, writable by the operator only.** The two problems above. Rejected for now, and it can be added
  on top of this list later if one is ever needed.
- **Resolve the name when the endpoint is registered and store the address.** Broken by a receiver whose address changes,
  and by DNS answering differently later (see 0003).
- **A separate connection pool for each allow list.** Closes the reuse gap and is far more code than the problem justifies.
