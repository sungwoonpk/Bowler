package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sungwoonpk/bowler/internal/deliver"
	"github.com/sungwoonpk/bowler/internal/dispatcher"
	"github.com/sungwoonpk/bowler/internal/ssrf"
	"github.com/sungwoonpk/bowler/internal/store"
	"github.com/sungwoonpk/bowler/retry"
	"github.com/sungwoonpk/bowler/signature"
)

const testKey = "test-api-key-0123456789abcdef"

var quick = retry.Policy{Schedule: []time.Duration{0, 30 * time.Millisecond, 30 * time.Millisecond}}

// receiver is a customer's server: it records what it is sent and answers as it is told to.
type receiver struct {
	srv    *httptest.Server
	mu     sync.Mutex
	hits   []received
	status int // 0 means 200
	fails  int // answer 500 this many times first
}

type received struct {
	id, timestamp, signature string
	body                     []byte
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	rc := &receiver{}
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.hits = append(rc.hits, received{r.Header.Get("webhook-id"), r.Header.Get("webhook-timestamp"), r.Header.Get("webhook-signature"), body})
		status := rc.status
		if rc.fails > 0 {
			rc.fails--
			status = 500
		}
		rc.mu.Unlock()
		if status == 0 {
			status = 200
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (r *receiver) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.hits) }

func (r *receiver) set(status, fails int) {
	r.mu.Lock()
	r.status, r.fails = status, fails
	r.mu.Unlock()
}

func (r *receiver) last() received { r.mu.Lock(); defer r.mu.Unlock(); return r.hits[len(r.hits)-1] }

type harness struct {
	t    *testing.T
	srv  *httptest.Server
	st   *store.Store
	disp *dispatcher.Dispatcher
}

type setup struct {
	policy     ssrf.Policy
	maxPayload int
	noWorker   bool
}

func newHarness(t *testing.T, opts ...func(*setup)) *harness {
	t.Helper()
	cfg := setup{policy: ssrf.Policy{AllowPrivate: true, AllowHTTP: true}}
	for _, o := range opts {
		o(&cfg)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "bowler.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	h := &harness{t: t, st: st}
	disp, err := dispatcher.New(st, deliver.New(deliver.Config{SSRF: cfg.policy, Timeout: time.Second}), dispatcher.Config{
		Retry: quick, PollInterval: 10 * time.Millisecond, AttemptTimeout: time.Second, LeaseFor: 6 * time.Second, DrainTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.disp = disp
	if !cfg.noWorker {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { disp.Run(ctx); close(done) }()
		t.Cleanup(func() { cancel(); <-done })
	}
	api, err := New(Config{Store: st, APIKey: testKey, SSRF: cfg.policy, Notify: disp.Notify, Stats: disp.Stats, MaxPayloadBytes: cfg.maxPayload, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	h.srv = httptest.NewServer(api.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

type reply struct {
	status  int
	body    map[string]any
	raw     []byte
	headers http.Header
}

func (h *harness) do(method, path string, body any, headers ...string) reply {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+testKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := reply{status: resp.StatusCode, raw: raw, headers: resp.Header}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func (h *harness) app(name string) string {
	h.t.Helper()
	r := h.do("POST", "/v1/apps", map[string]any{"name": name})
	if r.status != 201 {
		h.t.Fatalf("create app: %d %s", r.status, r.raw)
	}
	return str(r.body, "id")
}

func (h *harness) endpoint(app, url string, extra map[string]any) (id, secret string) {
	h.t.Helper()
	body := map[string]any{"url": url}
	for k, v := range extra {
		body[k] = v
	}
	r := h.do("POST", "/v1/apps/"+app+"/endpoints", body)
	if r.status != 201 {
		h.t.Fatalf("create endpoint: %d %s", r.status, r.raw)
	}
	return str(r.body, "id"), str(r.body, "secret")
}

func (h *harness) send(app, eventType string, payload any, headers ...string) reply {
	h.t.Helper()
	return h.do("POST", "/v1/apps/"+app+"/messages", map[string]any{"event_type": eventType, "payload": payload}, headers...)
}

func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) deliveryState(app, id string) string {
	return str(h.do("GET", "/v1/apps/"+app+"/deliveries/"+id, nil).body, "state")
}

func (h *harness) firstDelivery(app string, msg reply) string {
	ds, _ := msg.body["deliveries"].([]any)
	if len(ds) == 0 {
		h.t.Fatalf("the message has no deliveries: %s", msg.raw)
	}
	return str(ds[0].(map[string]any), "id")
}

// ---- tests ----

func TestNewRefusesAnUnsafeConfiguration(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "x.db"))
	defer st.Close()
	if _, err := New(Config{Store: st, APIKey: "short"}); err == nil {
		t.Fatal("a short API key must be refused")
	}
	if _, err := New(Config{APIKey: testKey}); err == nil {
		t.Fatal("a missing store must be refused")
	}
}

func TestEveryRouteNeedsTheAPIKey(t *testing.T) {
	h := newHarness(t)
	routes := []string{
		"POST /v1/apps", "GET /v1/apps", "GET /v1/apps/x", "POST /v1/apps/x/endpoints", "GET /v1/apps/x/endpoints",
		"POST /v1/apps/x/messages", "GET /v1/apps/x/messages", "GET /metrics",
		"POST /v1/apps/x/deliveries/dlv_aaaaaaaaaaaaaaaaaaaa/replay",
	}
	for _, rt := range routes {
		method, path, _ := strings.Cut(rt, " ")
		for name, auth := range map[string]string{"none": "", "wrong": "Bearer not-the-key-not-the-key-nope", "wrong scheme": "Basic " + testKey} {
			r := h.do(method, path, nil, "Authorization", auth)
			if r.status != http.StatusUnauthorized && r.status != http.StatusTooManyRequests {
				t.Errorf("%s with %s key: got %d", rt, name, r.status)
			}
		}
	}
}

func TestWrongKeysAreSlowedDownButTheRightKeyAlwaysWorks(t *testing.T) {
	h := newHarness(t)
	var got429 bool
	for range 15 {
		r := h.do("GET", "/v1/apps", nil, "Authorization", "Bearer wrong-wrong-wrong-wrong-wrong")
		if r.status == 429 {
			got429 = true
			if r.headers.Get("Retry-After") == "" {
				t.Error("a throttled reply must say when to come back")
			}
		}
	}
	if !got429 {
		t.Fatal("guessing must be cut off")
	}
	if r := h.do("GET", "/v1/apps", nil); r.status != 200 {
		t.Fatalf("a caller with the right key must never be locked out by someone else's guesses: %d", r.status)
	}
}

func TestErrorsAreJSONWithARequestID(t *testing.T) {
	h := newHarness(t)
	r := h.do("GET", "/v1/nothing-here", nil)
	if r.status != 404 || r.headers.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %s", r.status, r.headers.Get("Content-Type"))
	}
	e := r.body["error"].(map[string]any)
	if e["code"] != "not_found" || e["request_id"] == "" || e["request_id"] != r.headers.Get("X-Request-ID") {
		t.Fatalf("%v", e)
	}
	if r := h.do("DELETE", "/v1/apps", nil); r.status != 405 {
		t.Fatalf("a wrong method is 405, got %d", r.status)
	}
	if got := h.do("GET", "/healthz", nil, "X-Request-ID", "trace-42").headers.Get("X-Request-ID"); got != "trace-42" {
		t.Fatalf("a safe caller id is kept, got %q", got)
	}
	if got := h.do("GET", "/healthz", nil, "X-Request-ID", "bad id\twith space").headers.Get("X-Request-ID"); !strings.HasPrefix(got, "req_") {
		t.Fatalf("an unsafe caller id is replaced, got %q", got)
	}
	if r := h.do("GET", "/healthz", nil); r.headers.Get("X-Content-Type-Options") != "nosniff" || r.headers.Get("Cache-Control") != "no-store" {
		t.Fatal("responses must carry the safe headers")
	}
}

func TestAppsCreateResolveListAndValidate(t *testing.T) {
	h := newHarness(t)
	r := h.do("POST", "/v1/apps", map[string]any{"name": "  Acme  ", "uid": "acme-1"})
	if r.status != 201 || str(r.body, "name") != "Acme" || str(r.body, "uid") != "acme-1" {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	id := str(r.body, "id")
	if r := h.do("POST", "/v1/apps", map[string]any{"name": "dup", "uid": "acme-1"}); r.status != 409 {
		t.Fatalf("a repeated uid is a conflict, got %d", r.status)
	}
	for _, ref := range []string{id, "acme-1"} {
		if r := h.do("GET", "/v1/apps/"+ref, nil); r.status != 200 || str(r.body, "id") != id {
			t.Fatalf("resolve %q: %d", ref, r.status)
		}
	}
	if r := h.do("GET", "/v1/apps/ghost", nil); r.status != 404 {
		t.Fatalf("got %d", r.status)
	}

	bad := map[string]any{
		"empty name":     map[string]any{"name": "  "},
		"missing name":   map[string]any{},
		"long name":      map[string]any{"name": strings.Repeat("n", 101)},
		"control char":   map[string]any{"name": "bad\x07name"},
		"bad uid":        map[string]any{"name": "x", "uid": "has space"},
		"uid like an id": map[string]any{"name": "x", "uid": "app_abcdefghijklmnopqrst"},
		"unknown field":  map[string]any{"name": "x", "nmae": "typo"},
		"wrong type":     map[string]any{"name": 5},
	}
	for name, body := range bad {
		if r := h.do("POST", "/v1/apps", body); r.status != 400 {
			t.Errorf("%s: got %d %s", name, r.status, r.raw)
		}
	}
	if r := h.do("POST", "/v1/apps", "{not json"); r.status != 400 {
		t.Errorf("broken JSON: %d", r.status)
	}
	if r := h.do("POST", "/v1/apps", `{"name":"a"}{"name":"b"}`); r.status != 400 {
		t.Errorf("two values: %d", r.status)
	}
	if r := h.do("POST", "/v1/apps", ""); r.status != 400 {
		t.Errorf("empty body: %d", r.status)
	}
	if r := h.do("POST", "/v1/apps", `{"name":"a"}`, "Content-Type", "text/plain"); r.status != 415 {
		t.Errorf("wrong content type: %d", r.status)
	}
	if r := h.do("POST", "/v1/apps", `{"name":"`+strings.Repeat("a", 2<<20)+`"}`); r.status != 413 {
		t.Errorf("a huge body: %d", r.status)
	}
}

func TestPaginationWorksAndRefusesBadInput(t *testing.T) {
	h := newHarness(t)
	for i := range 7 {
		h.app(fmt.Sprintf("app-%d", i))
		time.Sleep(2 * time.Millisecond)
	}
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		path := "/v1/apps?limit=3"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		r := h.do("GET", path, nil)
		for _, it := range r.body["data"].([]any) {
			seen[str(it.(map[string]any), "id")] = true
		}
		cursor = str(r.body, "next_cursor")
		if cursor == "" {
			break
		}
	}
	if len(seen) != 7 {
		t.Fatalf("pages should add up to all 7 apps, saw %d", len(seen))
	}
	for _, q := range []string{"limit=0", "limit=251", "limit=abc", "cursor=%25%25%25"} {
		if r := h.do("GET", "/v1/apps?"+q, nil); r.status != 400 {
			t.Errorf("%s: got %d", q, r.status)
		}
	}
	if r := h.do("GET", "/v1/apps/"+h.app("empty")+"/endpoints", nil); r.status != 200 || fmt.Sprint(r.body["data"]) != "[]" {
		t.Errorf("an empty list is [] and not null: %s", r.raw)
	}
}

func TestEndpointURLsAreCheckedAgainstThePolicy(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.policy = ssrf.Policy{} }) // the strict, default policy
	app := h.app("a")
	for _, url := range []string{
		"http://example.com/hook", "https://127.0.0.1/hook", "https://localhost/hook", "https://169.254.169.254/latest",
		"https://10.0.0.5/hook", "https://user:pw@example.com/", "ftp://example.com/", "not a url", "",
	} {
		if r := h.do("POST", "/v1/apps/"+app+"/endpoints", map[string]any{"url": url}); r.status != 400 {
			t.Errorf("%q should be refused, got %d", url, r.status)
		}
	}
	if r := h.do("POST", "/v1/apps/"+app+"/endpoints", map[string]any{"url": "https://example.com/hook"}); r.status != 201 {
		t.Errorf("a public https URL is fine: %d %s", r.status, r.raw)
	}
}

func TestEndpointLifecycle(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	app := h.app("a")
	rc := newReceiver(t)

	r := h.do("POST", "/v1/apps/"+app+"/endpoints", map[string]any{"url": rc.srv.URL, "description": "billing", "event_types": []string{"invoice.paid"}, "concurrency": 3})
	if r.status != 201 || !strings.HasPrefix(str(r.body, "secret"), "whsec_") || r.body["concurrency"] != float64(3) {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	id, secret := str(r.body, "id"), str(r.body, "secret")
	base := "/v1/apps/" + app + "/endpoints/" + id

	// The secret is in the creation response and in the secret route, and nowhere else.
	if got := h.do("GET", base, nil); strings.Contains(string(got.raw), "whsec_") {
		t.Fatalf("a secret leaked into the endpoint view: %s", got.raw)
	}
	if got := h.do("GET", "/v1/apps/"+app+"/endpoints", nil); strings.Contains(string(got.raw), "whsec_") {
		t.Fatalf("a secret leaked into the list: %s", got.raw)
	}
	if got := h.do("GET", base+"/secret", nil); str(got.body, "secret") != secret {
		t.Fatalf("the secret route returns the same secret: %s", got.raw)
	}

	// Updates: set, change, and clear the event filter, which differ in JSON.
	if r := h.do("PATCH", base, map[string]any{"description": "payments", "concurrency": 5}); r.status != 200 || str(r.body, "description") != "payments" || fmt.Sprint(r.body["event_types"]) != "[invoice.paid]" {
		t.Fatalf("leaving event_types out leaves it alone: %d %s", r.status, r.raw)
	}
	if r := h.do("PATCH", base, map[string]any{"event_types": []string{"a", "b"}}); fmt.Sprint(r.body["event_types"]) != "[a b]" {
		t.Fatalf("%s", r.raw)
	}
	if r := h.do("PATCH", base, `{"event_types":null}`); r.status != 200 || r.body["event_types"] != nil {
		t.Fatalf("null clears the filter: %d %s", r.status, r.raw)
	}
	if r := h.do("PATCH", base, map[string]any{"disabled": true}); r.status != 200 || r.body["disabled_at"] == nil {
		t.Fatalf("%s", r.raw)
	}
	if r := h.do("PATCH", base, map[string]any{"disabled": false}); r.body["disabled_at"] != nil {
		t.Fatalf("%s", r.raw)
	}
	for name, body := range map[string]any{
		"bad url":          map[string]any{"url": "ftp://x"},
		"zero concurrency": map[string]any{"concurrency": 0},
		"huge concurrency": map[string]any{"concurrency": 99},
		"bad type":         map[string]any{"event_types": []string{"has space"}},
		"not a list":       `{"event_types": 5}`,
	} {
		if r := h.do("PATCH", base, body); r.status != 400 {
			t.Errorf("%s: got %d", name, r.status)
		}
	}

	// Rotation.
	rot := h.do("POST", base+"/secret/rotate", map[string]any{"grace_hours": 2})
	if rot.status != 200 || str(rot.body, "secret") == secret || rot.body["previous_secret_valid_until"] == nil {
		t.Fatalf("%d %s", rot.status, rot.raw)
	}
	if now := h.do("GET", base+"/secret", nil); str(now.body, "secret") != str(rot.body, "secret") {
		t.Fatal("the secret route returns the new secret after a rotation")
	}
	if r := h.do("POST", base+"/secret/rotate", map[string]any{"grace_hours": 0}); r.body["previous_secret_valid_until"] != nil {
		t.Errorf("no grace period, so no expiry to report: %s", r.raw)
	}
	if r := h.do("POST", base+"/secret/rotate", nil); r.status != 200 {
		t.Errorf("the body is optional: %d", r.status)
	}
	if r := h.do("POST", base+"/secret/rotate", map[string]any{"grace_hours": 9999}); r.status != 400 {
		t.Errorf("a grace period past the limit: %d", r.status)
	}

	// Delete.
	if r := h.do("DELETE", base, nil); r.status != 204 {
		t.Fatalf("delete: %d", r.status)
	}
	for _, m := range []string{"GET", "PATCH", "DELETE"} {
		var body any
		if m == "PATCH" {
			body = map[string]any{"description": "x"}
		}
		if r := h.do(m, base, body); r.status != 404 {
			t.Errorf("%s after delete: %d", m, r.status)
		}
	}
}

func TestMessageIsDeliveredSignedAndRecorded(t *testing.T) {
	h := newHarness(t)
	app := h.app("a")
	rc := newReceiver(t)
	_, secret := h.endpoint(app, rc.srv.URL, nil)

	payload := map[string]any{"invoice": "in_1", "amount": 4200}
	msg := h.send(app, "invoice.paid", payload)
	if msg.status != 202 || str(msg.body, "event_type") != "invoice.paid" {
		t.Fatalf("%d %s", msg.status, msg.raw)
	}
	delivery := h.firstDelivery(app, msg)

	eventually(t, 3*time.Second, "the receiver to be called", func() bool { return rc.count() == 1 })
	got := rc.last()
	if got.id != str(msg.body, "id") {
		t.Fatalf("webhook-id must be the message id: %q vs %q", got.id, str(msg.body, "id"))
	}
	if err := signature.Verify([]string{secret}, got.id, got.timestamp, got.signature, got.body, time.Now(), 0); err != nil {
		t.Fatalf("the customer could not verify the signature: %v", err)
	}
	var sent map[string]any
	json.Unmarshal(got.body, &sent)
	if sent["invoice"] != "in_1" || sent["amount"] != float64(4200) {
		t.Fatalf("the payload must arrive as sent: %s", got.body)
	}

	eventually(t, 3*time.Second, "the delivery to be marked succeeded", func() bool { return h.deliveryState(app, delivery) == "succeeded" })
	atts := h.do("GET", "/v1/apps/"+app+"/deliveries/"+delivery+"/attempts", nil).body["data"].([]any)
	if len(atts) != 1 || atts[0].(map[string]any)["status_code"] != float64(200) {
		t.Fatalf("the attempt should be recorded: %v", atts)
	}
	if _, has := atts[0].(map[string]any)["duration_ms"]; !has {
		t.Fatal("an attempt reports its duration in milliseconds")
	}
	if r := h.do("GET", "/v1/apps/"+app+"/messages/"+str(msg.body, "id")+"/deliveries", nil); len(r.body["data"].([]any)) != 1 {
		t.Fatalf("%s", r.raw)
	}
}

func TestIdempotencyKeyStopsDoubleSending(t *testing.T) {
	h := newHarness(t)
	app := h.app("a")
	rc := newReceiver(t)
	h.endpoint(app, rc.srv.URL, nil)

	first := h.send(app, "x", map[string]any{"n": 1}, "Idempotency-Key", "order-77")
	second := h.send(app, "x", map[string]any{"n": 1}, "Idempotency-Key", "order-77")
	if first.status != 202 || second.status != 202 || str(first.body, "id") != str(second.body, "id") {
		t.Fatalf("a repeat returns the original message: %s / %s", first.raw, second.raw)
	}
	if second.headers.Get("Idempotent-Replayed") != "true" || first.headers.Get("Idempotent-Replayed") != "" {
		t.Fatal("only the repeat is marked as a replay")
	}
	eventually(t, 3*time.Second, "the first delivery", func() bool { return rc.count() >= 1 })
	time.Sleep(150 * time.Millisecond)
	if rc.count() != 1 {
		t.Fatalf("the receiver must hear about it once, heard %d times", rc.count())
	}
	if r := h.send(app, "x", map[string]any{}, "Idempotency-Key", strings.Repeat("k", 256)); r.status != 400 {
		t.Errorf("a key that is too long: %d", r.status)
	}
}

func TestEventTypeFilterDecidesWhoGetsWhat(t *testing.T) {
	h := newHarness(t)
	app := h.app("a")
	paid, all := newReceiver(t), newReceiver(t)
	h.endpoint(app, paid.srv.URL, map[string]any{"event_types": []string{"invoice.paid"}})
	h.endpoint(app, all.srv.URL, nil)

	h.send(app, "invoice.created", map[string]any{})
	h.send(app, "invoice.paid", map[string]any{})
	eventually(t, 3*time.Second, "both receivers to hear what they asked for", func() bool { return all.count() == 2 && paid.count() == 1 })
	time.Sleep(100 * time.Millisecond)
	if paid.count() != 1 {
		t.Fatalf("the filtered endpoint must only get invoice.paid, got %d", paid.count())
	}
}

func TestFailuresAreRetriedAndShowTheirReason(t *testing.T) {
	h := newHarness(t)
	app := h.app("a")
	rc := newReceiver(t)
	rc.set(0, 2) // two failures, then fine
	h.endpoint(app, rc.srv.URL, nil)
	msg := h.send(app, "x", map[string]any{})
	delivery := h.firstDelivery(app, msg)

	eventually(t, 4*time.Second, "success after two failures", func() bool { return h.deliveryState(app, delivery) == "succeeded" })
	atts := h.do("GET", "/v1/apps/"+app+"/deliveries/"+delivery+"/attempts", nil).body["data"].([]any)
	if len(atts) != 3 {
		t.Fatalf("two failures and a success: %v", atts)
	}
	first := atts[0].(map[string]any)
	if first["status_code"] != float64(500) || first["failure"] != "http_5xx" {
		t.Fatalf("a failure says what went wrong: %v", first)
	}
}

func TestAFailedDeliveryCanBeReplayed(t *testing.T) {
	h := newHarness(t)
	app := h.app("a")
	rc := newReceiver(t)
	rc.set(500, 0)
	h.endpoint(app, rc.srv.URL, nil)
	msg := h.send(app, "x", map[string]any{})
	delivery := h.firstDelivery(app, msg)
	eventually(t, 4*time.Second, "the delivery to fail for good", func() bool { return h.deliveryState(app, delivery) == "failed" })

	rc.set(200, 0) // the customer fixes their server
	replay := h.do("POST", "/v1/apps/"+app+"/deliveries/"+delivery+"/replay", nil)
	if replay.status != 202 {
		t.Fatalf("%d %s", replay.status, replay.raw)
	}
	eventually(t, 3*time.Second, "the replay to succeed", func() bool { return h.deliveryState(app, delivery) == "succeeded" })
	atts := h.do("GET", "/v1/apps/"+app+"/deliveries/"+delivery+"/attempts", nil).body["data"].([]any)
	if last := atts[len(atts)-1].(map[string]any); last["trigger"] != "manual" {
		t.Fatalf("the replayed attempt is marked manual: %v", last)
	}
	if r := h.do("POST", "/v1/apps/"+app+"/deliveries/"+delivery+"/replay", nil); r.status != 202 {
		t.Fatalf("a succeeded delivery may also be sent again: %d", r.status)
	}
}

func TestReplayRefusesADeliveryThatIsStillWorking(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	app := h.app("a")
	rc := newReceiver(t)
	h.endpoint(app, rc.srv.URL, nil)
	delivery := h.firstDelivery(app, h.send(app, "x", map[string]any{}))
	if r := h.do("POST", "/v1/apps/"+app+"/deliveries/"+delivery+"/replay", nil); r.status != 409 {
		t.Fatalf("a delivery that is waiting is already on its way: %d %s", r.status, r.raw)
	}
}

func TestGoneSwitchesTheEndpointOffAndRecoverBringsItBack(t *testing.T) {
	h := newHarness(t)
	app := h.app("a")
	rc := newReceiver(t)
	rc.set(410, 0)
	ep, _ := h.endpoint(app, rc.srv.URL, nil)
	delivery := h.firstDelivery(app, h.send(app, "x", map[string]any{}))
	eventually(t, 3*time.Second, "the delivery to fail on a 410", func() bool { return h.deliveryState(app, delivery) == "failed" })

	got := h.do("GET", "/v1/apps/"+app+"/endpoints/"+ep, nil)
	if got.body["disabled_at"] == nil || !strings.Contains(str(got.body, "disabled_reason"), "410") {
		t.Fatalf("a 410 switches the endpoint off, with the reason: %s", got.raw)
	}
	if ds := h.send(app, "y", map[string]any{}).body["deliveries"].([]any); len(ds) != 0 {
		t.Fatalf("a disabled endpoint gets nothing new: %v", ds)
	}

	rc.set(200, 0)
	rec := h.do("POST", "/v1/apps/"+app+"/endpoints/"+ep+"/recover", map[string]any{"since": time.Now().Add(-time.Hour).Format(time.RFC3339)})
	if rec.status != 200 || rec.body["replayed"] != float64(2) {
		t.Fatalf("one failed delivery replayed and one missed message sent, so two: %d %s", rec.status, rec.raw)
	}
	eventually(t, 3*time.Second, "the failed delivery to go through", func() bool { return h.deliveryState(app, delivery) == "succeeded" })
	eventually(t, 3*time.Second, "the message sent while the endpoint was off to arrive too", func() bool {
		r := h.do("GET", "/v1/apps/"+app+"/endpoints/"+ep+"/deliveries?state=succeeded", nil)
		data, _ := r.body["data"].([]any)
		return len(data) == 2
	})
	if h.do("GET", "/v1/apps/"+app+"/endpoints/"+ep, nil).body["disabled_at"] != nil {
		t.Fatal("recovering switches the endpoint back on")
	}
	if r := h.do("POST", "/v1/apps/"+app+"/endpoints/"+ep+"/recover", nil); r.status != 200 || r.body["replayed"] != float64(0) {
		t.Fatalf("recovering with no body uses the last day and may find nothing: %d %s", r.status, r.raw)
	}
	if r := h.do("POST", "/v1/apps/"+app+"/endpoints/"+ep+"/recover", `{"since":"yesterday"}`); r.status != 400 {
		t.Fatalf("a bad time: %d", r.status)
	}
}

func TestOneAppCannotSeeAnotherApp(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	a, b := h.app("a"), h.app("b")
	rc := newReceiver(t)
	ep, _ := h.endpoint(a, rc.srv.URL, nil)
	msg := h.send(a, "x", map[string]any{})
	delivery := h.firstDelivery(a, msg)

	for _, path := range []string{
		"/v1/apps/" + b + "/endpoints/" + ep,
		"/v1/apps/" + b + "/endpoints/" + ep + "/secret",
		"/v1/apps/" + b + "/endpoints/" + ep + "/deliveries",
		"/v1/apps/" + b + "/messages/" + str(msg.body, "id"),
		"/v1/apps/" + b + "/messages/" + str(msg.body, "id") + "/deliveries",
		"/v1/apps/" + b + "/deliveries/" + delivery,
		"/v1/apps/" + b + "/deliveries/" + delivery + "/attempts",
	} {
		if r := h.do("GET", path, nil); r.status != 404 {
			t.Errorf("GET %s: another app must get 404, got %d", path, r.status)
		}
	}
	if r := h.do("PATCH", "/v1/apps/"+b+"/endpoints/"+ep, map[string]any{"description": "hijack"}); r.status != 404 {
		t.Errorf("PATCH: %d", r.status)
	}
	if r := h.do("DELETE", "/v1/apps/"+b+"/endpoints/"+ep, nil); r.status != 404 {
		t.Errorf("DELETE: %d", r.status)
	}
	if r := h.do("POST", "/v1/apps/"+b+"/deliveries/"+delivery+"/replay", nil); r.status != 404 {
		t.Errorf("replay: %d", r.status)
	}
	// And nothing happened to the real one.
	if r := h.do("GET", "/v1/apps/"+a+"/endpoints/"+ep, nil); r.status != 200 || str(r.body, "description") != "" {
		t.Errorf("the owner's endpoint is untouched: %s", r.raw)
	}
}

func TestMessageValidation(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true; s.maxPayload = 100 })
	app := h.app("a")
	cases := map[string]string{
		"no event type":   `{"payload":{}}`,
		"bad event type":  `{"event_type":"has space","payload":{}}`,
		"long event type": `{"event_type":"` + strings.Repeat("e", 129) + `","payload":{}}`,
		"no payload":      `{"event_type":"x"}`,
		"null payload":    `{"event_type":"x","payload":null}`,
		"unknown field":   `{"event_type":"x","payload":{},"extra":1}`,
		"broken payload":  `{"event_type":"x","payload":{"a":}}`,
	}
	for name, body := range cases {
		if r := h.do("POST", "/v1/apps/"+app+"/messages", body); r.status != 400 {
			t.Errorf("%s: got %d %s", name, r.status, r.raw)
		}
	}
	big := `{"event_type":"x","payload":{"data":"` + strings.Repeat("d", 200) + `"}}`
	if r := h.do("POST", "/v1/apps/"+app+"/messages", big); r.status != 413 || str(r.body["error"].(map[string]any), "code") != "payload_too_large" {
		t.Errorf("a payload over the limit: %d %s", r.status, r.raw)
	}
	for _, p := range []string{`"a string"`, `42`, `[1,2]`, `true`} {
		if r := h.do("POST", "/v1/apps/"+app+"/messages", `{"event_type":"x","payload":`+p+`}`); r.status != 202 {
			t.Errorf("any JSON value except null may be a payload, %s gave %d", p, r.status)
		}
	}
}

func TestListingMessages(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	app := h.app("a")
	for i := range 5 {
		typ := "a"
		if i%2 == 1 {
			typ = "b"
		}
		h.send(app, typ, map[string]any{"i": i})
		time.Sleep(2 * time.Millisecond)
	}
	all := h.do("GET", "/v1/apps/"+app+"/messages?limit=3", nil)
	if len(all.body["data"].([]any)) != 3 || str(all.body, "next_cursor") == "" {
		t.Fatalf("%s", all.raw)
	}
	if only := h.do("GET", "/v1/apps/"+app+"/messages?event_type=b", nil); len(only.body["data"].([]any)) != 2 {
		t.Fatalf("%s", only.raw)
	}
	if r := h.do("GET", "/v1/apps/"+app+"/messages?event_type=bad%20type", nil); r.status != 400 {
		t.Fatalf("%d", r.status)
	}
	if r := h.do("GET", "/v1/apps/"+app+"/endpoints/ep_aaaaaaaaaaaaaaaaaaaa/deliveries?state=weird", nil); r.status != 400 && r.status != 404 {
		t.Fatalf("%d", r.status)
	}
}

func TestMalformedIDsAreNotFoundWithoutTouchingTheDatabase(t *testing.T) {
	h := newHarness(t)
	app := h.app("a")
	for _, path := range []string{
		"/v1/apps/" + app + "/endpoints/not-an-id", "/v1/apps/" + app + "/messages/msg_short", "/v1/apps/" + app + "/deliveries/ep_aaaaaaaaaaaaaaaaaaaa",
		"/v1/apps/" + app + "/endpoints/' OR 1=1 --", "/v1/apps/" + app + "/messages/..%2F..%2Fetc%2Fpasswd",
	} {
		if r := h.do("GET", path, nil); r.status != 404 {
			t.Errorf("%s: got %d", path, r.status)
		}
	}
}

func TestMetricsAndHealth(t *testing.T) {
	h := newHarness(t)
	app := h.app("a")
	rc := newReceiver(t)
	h.endpoint(app, rc.srv.URL, nil)
	delivery := h.firstDelivery(app, h.send(app, "x", map[string]any{}))
	eventually(t, 3*time.Second, "a delivery", func() bool { return h.deliveryState(app, delivery) == "succeeded" })

	m := h.do("GET", "/metrics", nil)
	text := string(m.raw)
	for _, want := range []string{`bowler_deliveries{state="succeeded"} 1`, "bowler_attempts_total{result=\"succeeded\"} 1", "bowler_claimed_total 1", `bowler_build_info{version="test"} 1`} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics should contain %q:\n%s", want, text)
		}
	}
	for _, p := range []string{"/healthz", "/readyz"} {
		req, _ := http.NewRequest("GET", h.srv.URL+p, nil) // no key: these are for load balancers
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Errorf("%s must answer without a key: %v %v", p, resp, err)
		}
		resp.Body.Close()
	}
}

func TestInternalErrorsDoNotLeakDetails(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	h.st.Close() // every query now fails
	r := h.do("GET", "/v1/apps", nil)
	if r.status != 500 {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	body := strings.ToLower(string(r.raw))
	for _, leak := range []string{"sql", "database", "closed", "select", "apps", ".db"} {
		if strings.Contains(body, leak) {
			t.Fatalf("an internal error must not reveal %q: %s", leak, r.raw)
		}
	}
	if rd := h.do("GET", "/readyz", nil); rd.status != 503 {
		t.Fatalf("readiness fails when the database does: %d", rd.status)
	}
}

func TestListingEndpointsDeliveriesAndReadingAMessage(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	app := h.app("a")
	rc := newReceiver(t)
	var first string
	for i := range 4 {
		id, _ := h.endpoint(app, rc.srv.URL, map[string]any{"description": fmt.Sprintf("ep %d", i)})
		if i == 0 {
			first = id
		}
		time.Sleep(2 * time.Millisecond)
	}
	page := h.do("GET", "/v1/apps/"+app+"/endpoints?limit=3", nil)
	if len(page.body["data"].([]any)) != 3 || str(page.body, "next_cursor") == "" {
		t.Fatalf("%s", page.raw)
	}
	rest := h.do("GET", "/v1/apps/"+app+"/endpoints?limit=3&cursor="+str(page.body, "next_cursor"), nil)
	if len(rest.body["data"].([]any)) != 1 || str(rest.body, "next_cursor") != "" {
		t.Fatalf("%s", rest.raw)
	}
	if r := h.do("GET", "/v1/apps/"+app+"/endpoints?limit=0", nil); r.status != 400 {
		t.Fatalf("%d", r.status)
	}

	msg := h.send(app, "invoice.paid", map[string]any{"n": 1})
	got := h.do("GET", "/v1/apps/"+app+"/messages/"+str(msg.body, "id"), nil)
	if got.status != 200 || str(got.body, "event_type") != "invoice.paid" {
		t.Fatalf("reading a message back: %d %s", got.status, got.raw)
	}
	for i := range 3 {
		h.send(app, "x", map[string]any{"i": i})
	}
	pending := h.do("GET", "/v1/apps/"+app+"/endpoints/"+first+"/deliveries?state=pending&limit=2", nil)
	if len(pending.body["data"].([]any)) != 2 || str(pending.body, "next_cursor") == "" {
		t.Fatalf("%s", pending.raw)
	}
	if more := h.do("GET", "/v1/apps/"+app+"/endpoints/"+first+"/deliveries?state=pending&limit=2&cursor="+str(pending.body, "next_cursor"), nil); len(more.body["data"].([]any)) != 2 {
		t.Fatalf("%s", more.raw)
	}
	if none := h.do("GET", "/v1/apps/"+app+"/endpoints/"+first+"/deliveries?state=succeeded", nil); len(none.body["data"].([]any)) != 0 {
		t.Fatalf("%s", none.raw)
	}
	if r := h.do("GET", "/v1/apps/"+app+"/endpoints/"+first+"/deliveries?state=weird", nil); r.status != 400 {
		t.Fatalf("an unknown state is a 400: %d", r.status)
	}
	if r := h.do("GET", "/v1/apps/"+app+"/endpoints/"+first+"/deliveries?cursor=%25%25", nil); r.status != 400 {
		t.Fatalf("a bad cursor is a 400: %d", r.status)
	}
}

func TestEndpointCreationValidation(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	app := h.app("a")
	rc := newReceiver(t)
	cases := map[string]map[string]any{
		"long description":     {"url": rc.srv.URL, "description": strings.Repeat("d", 501)},
		"bad event type":       {"url": rc.srv.URL, "event_types": []string{"has space"}},
		"too many types":       {"url": rc.srv.URL, "event_types": make([]string, 51)},
		"negative concurrency": {"url": rc.srv.URL, "concurrency": -1},
		"huge concurrency":     {"url": rc.srv.URL, "concurrency": 21},
	}
	for name, body := range cases {
		if r := h.do("POST", "/v1/apps/"+app+"/endpoints", body); r.status != 400 {
			t.Errorf("%s: got %d", name, r.status)
		}
	}
	if r := h.do("POST", "/v1/apps/ghost/endpoints", map[string]any{"url": rc.srv.URL}); r.status != 404 {
		t.Errorf("an endpoint for an app that does not exist: %d", r.status)
	}
	r := h.do("POST", "/v1/apps/"+app+"/endpoints", map[string]any{"url": rc.srv.URL, "event_types": []string{}})
	if r.status != 201 || fmt.Sprint(r.body["event_types"]) != "[]" {
		t.Errorf("an empty list means the endpoint takes no events at all, which is not the same as every event: %d %s", r.status, r.raw)
	}
}
