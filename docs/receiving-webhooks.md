# For the customers who receive webhooks

What the people at the other end need to know: what arrives, how to check it, and what to answer.

Each request has three headers, and the body is exactly the payload you sent.

```
webhook-id:         msg_...            the message id, the same on every attempt
webhook-timestamp:  1731403200         when this attempt was made
webhook-signature:  v1,g0hM9Ss...      one or two signatures
```

A Go receiver verifies in one call. This is the tested example from the `signature` package:

```go
body, _ := io.ReadAll(r.Body) // read it before parsing: the signature covers these exact bytes
err := signature.Verify(
    []string{secret}, // pass the old secret too while one is being rotated
    r.Header.Get(signature.HeaderID),
    r.Header.Get(signature.HeaderTimestamp),
    r.Header.Get(signature.HeaderSignature),
    body, time.Now(), 0, // 0 means the default tolerance of five minutes
)
if err != nil {
    http.Error(w, "invalid signature", http.StatusBadRequest)
    return
}
// Ignore a webhook-id you have already handled, then do the work.
```

Receivers in other languages can use any Standard Webhooks library. Answer `2xx` to accept. Anything else is retried, except `410 Gone`, which switches the endpoint off. Because delivery is at least once, **ignore a `webhook-id` you have seen before**.
