# Using the API

The full description is [openapi.json](openapi.json). It covers every route and the webhook your customers receive, and a test fails if it drifts from the server.

**Set up an app and send a message:**

```bash
curl -s localhost:8080/v1/apps -H "authorization: Bearer $BOWLER_API_KEY" -H "content-type: application/json" \
  -d '{"name": "Acme", "uid": "acme-1"}'

curl -s localhost:8080/v1/apps/acme-1/endpoints -H "authorization: Bearer $BOWLER_API_KEY" -H "content-type: application/json" \
  -d '{"url": "https://hooks.example.com/bowler", "event_types": ["invoice.paid"], "concurrency": 3, "rate_limit_per_second": 20}'
# the response includes the signing "secret"

curl -s localhost:8080/v1/apps/acme-1/messages -H "authorization: Bearer $BOWLER_API_KEY" -H "content-type: application/json" \
  -H "Idempotency-Key: invoice-1042" \
  -d '{"event_type": "invoice.paid", "payload": {"invoice": "in_1042", "amount": 4200}}'
```

**Give an app's own side a token** that works on that app only, such as for a page where your customer manages its endpoints:

```bash
curl -s localhost:8080/v1/apps/acme-1/tokens -H "authorization: Bearer $BOWLER_API_KEY" -H "content-type: application/json" \
  -d '{"name": "customer portal", "scope": "read", "expires_in_hours": 720}'
# scope and expiry are optional. The response includes "token": "bwl_...", shown once. Only its hash is kept.

curl -s localhost:8080/v1/apps/acme-1/endpoints -H "authorization: Bearer bwl_..."    # works
curl -s -X POST localhost:8080/v1/apps/acme-1/endpoints -H "authorization: Bearer bwl_..." -d '{}'   # 403 read_only, this token is read only
curl -s localhost:8080/v1/apps/other-app/endpoints -H "authorization: Bearer bwl_..."  # 404, as if the app did not exist
curl -s localhost:8080/v1/apps/acme-1/audit-log -H "authorization: Bearer bwl_..."     # who did what
curl -s -X DELETE localhost:8080/v1/apps/acme-1/tokens/tok_... -H "authorization: Bearer $BOWLER_API_KEY"   # revoked at once
```

**After a customer fixes their server:**

```bash
curl -s -X POST localhost:8080/v1/apps/acme-1/endpoints/ep_.../recover -H "authorization: Bearer $BOWLER_API_KEY"
```

This switches the endpoint on, replays deliveries that failed in the last day, and sends the messages that arrived while it was off.
