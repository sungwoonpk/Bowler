# 0002. Webhooks are signed as the Standard Webhooks specification says

Status: accepted

## Context

A customer's server must be able to tell that a request really came from Bowler, that nobody changed it on the way, and
that it is not an old request being sent again. Every webhook sender invents its own scheme for this, and every
receiver has to learn each one.

## Decision

- Follow the open [Standard Webhooks](https://www.standardwebhooks.com) specification instead of inventing a format.
  Three headers carry everything: `webhook-id`, `webhook-timestamp` and `webhook-signature`.
- The signature is `v1,` plus the base64 HMAC-SHA256 of `<id>.<timestamp>.<body>`. Because the id, the time and the
  body are all covered, changing any of them breaks it.
- The key is the secret, `whsec_` followed by 24 random bytes in base64, one per endpoint.
- **The timestamp is the time of the attempt**, so each retry carries a fresh one. A receiver can reject anything far from
  its own clock, which limits how long a captured request can be replayed.
- **The id is the message id and does not change between retries.** That is what lets a receiver drop a repeat.
- **Rotation sends two signatures** for a grace period, one per secret, in the same header. A receiver on the old
  secret and one already on the new secret both accept the same request, so nobody has to switch at the same moment.
- Verification is a **public package** (`signature`) with no dependencies, so a Go receiver can verify in one call and
  a receiver in any language can use the specification's own libraries.
- Comparison uses `hmac.Equal`, which takes the same time however much of a guess is right.
- The test uses the specification's published vector, and that vector was reproduced independently with `openssl`
  before it was trusted.

## Costs

- The secret must be **readable** to sign with, so it is stored as it is, not as a hash. See the threat model.
- A receiver whose clock is badly wrong rejects every webhook. That is the cost of replay protection.
- The signature covers the body byte for byte, so a proxy that reformats JSON on the way breaks it. The body is sent
  exactly as it was received from the caller to make this less likely.

## Alternatives considered

- **A custom header and scheme.** Free to design, and every customer has to learn it.
- **Asymmetric signatures (an Ed25519 key pair).** The receiver would hold only a public key, which is a real advantage.
  The specification supports it as `v1a`, and it is a good future addition. HMAC was chosen first because it is what
  nearly every receiver library supports today.
- **No signature, only a secret URL.** The URL ends up in logs and proxies, and nothing proves the body was not changed.
