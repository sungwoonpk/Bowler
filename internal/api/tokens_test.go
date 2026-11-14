package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func bearer(tok string) []string { return []string{"Authorization", "Bearer " + tok} }

// token makes a token for an app and returns it with its id.
func (h *harness) token(app, name string) (secret, id string) {
	h.t.Helper()
	r := h.do("POST", "/v1/apps/"+app+"/tokens", map[string]any{"name": name})
	if r.status != 201 {
		h.t.Fatalf("making a token: %d %s", r.status, r.raw)
	}
	return str(r.body, "token"), str(r.body, "id")
}

func TestATokenIsShownOnceAndOnlyItsHashIsKept(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	app := h.app("a")
	r := h.do("POST", "/v1/apps/"+app+"/tokens", map[string]any{"name": "portal"})
	secret := str(r.body, "token")
	if r.status != 201 || !strings.HasPrefix(secret, "bwl_") || len(secret) != 56 || str(r.body, "name") != "portal" || str(r.body, "prefix") != secret[:10] {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	id := str(r.body, "id")
	for _, path := range []string{"/v1/apps/" + app + "/tokens"} {
		if got := h.do("GET", path, nil); strings.Contains(string(got.raw), secret[10:]) || got.status != 200 {
			t.Fatalf("the token must not appear again: %d %s", got.status, got.raw)
		}
	}
	if got := h.do("GET", "/v1/audit-log", nil); strings.Contains(string(got.raw), secret[10:]) {
		t.Fatalf("the token must not be in the audit log: %s", got.raw)
	}
	_ = id
}

func TestATokenActsOnItsOwnAppAndOnNoOther(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	a, b := h.app("a"), h.app("b")
	rc := newReceiver(t)
	tok, _ := h.token(a, "portal")

	if r := h.do("GET", "/v1/apps/"+a, nil, bearer(tok)...); r.status != 200 {
		t.Fatalf("its own app: %d %s", r.status, r.raw)
	}
	ep := h.do("POST", "/v1/apps/"+a+"/endpoints", map[string]any{"url": rc.srv.URL}, bearer(tok)...)
	if ep.status != 201 {
		t.Fatalf("a token creates endpoints on its app: %d %s", ep.status, ep.raw)
	}
	if r := h.send(a, "x", map[string]any{}, bearer(tok)...); r.status != 202 {
		t.Fatalf("and sends messages: %d %s", r.status, r.raw)
	}
	// Another app looks exactly like one that does not exist.
	other := h.do("GET", "/v1/apps/"+b+"/endpoints", nil, bearer(tok)...)
	ghost := h.do("GET", "/v1/apps/app_aaaaaaaaaaaaaaaaaaaa/endpoints", nil, bearer(tok)...)
	if other.status != 404 || ghost.status != 404 || string(other.raw) != string(ghost.raw) && str(other.body, "code") != str(ghost.body, "code") {
		t.Fatalf("another app must be a plain 404, same as a missing one: %d %s / %d %s", other.status, other.raw, ghost.status, ghost.raw)
	}
}

// Every route that names an app must refuse a token made for a different one. Checking each route by hand would miss the
// one added next year, so this walks the routes the server registers.
func TestNoRouteLetsATokenReachAnotherApp(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	a, b := h.app("a"), h.app("b")
	rc := newReceiver(t)
	epB, _ := h.endpoint(b, rc.srv.URL, nil)
	tok, _ := h.token(a, "portal")
	fill := strings.NewReplacer("{app}", b, "{endpoint}", epB, "{message}", "msg_aaaaaaaaaaaaaaaaaaaa",
		"{delivery}", "dlv_aaaaaaaaaaaaaaaaaaaa", "{token}", "tok_aaaaaaaaaaaaaaaaaaaa")

	checked := 0
	for _, route := range h.api.Routes() {
		method, path, _ := strings.Cut(route, " ")
		if path == "/healthz" || path == "/readyz" {
			continue
		}
		var body any
		if method == "POST" || method == "PATCH" {
			body = map[string]any{}
		}
		r := h.do(method, fill.Replace(path), body, bearer(tok)...)
		switch {
		case strings.Contains(path, "{app}"):
			if r.status != 404 && r.status != 403 {
				t.Errorf("%s with a token of another app answered %d: %s", route, r.status, r.raw)
			}
		default:
			if r.status != 403 {
				t.Errorf("%s with a token answered %d, the operator's routes must refuse it: %s", route, r.status, r.raw)
			}
		}
		checked++
	}
	if checked < 20 {
		t.Fatalf("only %d routes were checked, the walk is not seeing the routes", checked)
	}
}

func TestATokenCannotMakeOrListOrRevokeTokens(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	a := h.app("a")
	tok, id := h.token(a, "portal")
	for name, req := range map[string][3]any{
		"make":   {"POST", "/v1/apps/" + a + "/tokens", map[string]any{"name": "mine"}},
		"list":   {"GET", "/v1/apps/" + a + "/tokens", nil},
		"revoke": {"DELETE", "/v1/apps/" + a + "/tokens/" + id, nil},
	} {
		if r := h.do(req[0].(string), req[1].(string), req[2], bearer(tok)...); r.status != 403 {
			t.Errorf("%s: a token must not manage tokens, got %d", name, r.status)
		}
	}
	// And a refusal is not a wrong key, so it does not count toward the throttle.
	for range 15 {
		h.do("GET", "/v1/apps", nil, bearer(tok)...)
	}
	if r := h.do("GET", "/v1/apps/"+a, nil, bearer(tok)...); r.status != 200 {
		t.Fatalf("a valid token must not be throttled for asking for something it may not have: %d", r.status)
	}
}

func TestARevokedTokenStopsAtOnceAndRevokingTwiceIsFine(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	a := h.app("a")
	tok, id := h.token(a, "portal")
	if r := h.do("GET", "/v1/apps/"+a, nil, bearer(tok)...); r.status != 200 {
		t.Fatalf("setup: %d", r.status)
	}
	r := h.do("DELETE", "/v1/apps/"+a+"/tokens/"+id, nil)
	if r.status != 200 || r.body["revoked_at"] == nil {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	if r := h.do("GET", "/v1/apps/"+a, nil, bearer(tok)...); r.status != 401 {
		t.Fatalf("a revoked token must stop working at once, got %d", r.status)
	}
	if again := h.do("DELETE", "/v1/apps/"+a+"/tokens/"+id, nil); again.status != 200 || again.body["revoked_at"] != r.body["revoked_at"] {
		t.Fatalf("revoking again changes nothing: %d %s", again.status, again.raw)
	}
	list := h.do("GET", "/v1/apps/"+a+"/tokens", nil)
	if data, _ := list.body["data"].([]any); len(data) != 1 {
		t.Fatalf("a revoked token stays in the list: %s", list.raw)
	}
}

func TestAWrongOrUnknownTokenIsRefusedAndThrottledLikeAWrongKey(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	a := h.app("a")
	good, _ := h.token(a, "portal")
	unknown := "bwl_" + strings.Repeat("a", 52)
	for _, c := range []string{unknown, good + "x", good[:30], "bwl_", "Bearer"} {
		if r := h.do("GET", "/v1/apps/"+a, nil, bearer(c)...); r.status != 401 {
			t.Errorf("%q: got %d", c, r.status)
		}
	}
	for range 12 {
		h.do("GET", "/v1/apps/"+a, nil, bearer(unknown)...)
	}
	if r := h.do("GET", "/v1/apps/"+a, nil, bearer(unknown)...); r.status != 429 {
		t.Fatalf("a flood of wrong tokens must be slowed down: %d", r.status)
	}
	if r := h.do("GET", "/v1/apps/"+a, nil, bearer(good)...); r.status != 200 {
		t.Fatalf("a right token still works while the address is slowed down: %d", r.status)
	}
}

func TestTokenCreationIsValidatedAndBounded(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	a := h.app("a")
	for name, body := range map[string]any{
		"no name": map[string]any{}, "blank": map[string]any{"name": "  "}, "long": map[string]any{"name": strings.Repeat("n", 101)},
		"control": map[string]any{"name": "a\x07b"}, "unknown field": map[string]any{"name": "x", "app_id": "other"},
	} {
		if r := h.do("POST", "/v1/apps/"+a+"/tokens", body); r.status != 400 {
			t.Errorf("%s: got %d", name, r.status)
		}
	}
	if r := h.do("POST", "/v1/apps/ghost/tokens", map[string]any{"name": "x"}); r.status != 404 {
		t.Errorf("a token for an app that does not exist: %d", r.status)
	}
	for i := range 50 {
		h.token(a, fmt.Sprint("t", i))
	}
	if r := h.do("POST", "/v1/apps/"+a+"/tokens", map[string]any{"name": "one too many"}); r.status != 409 || str(r.body["error"].(map[string]any), "code") != "too_many_tokens" {
		t.Fatalf("%d %s", r.status, r.raw)
	}
}

func entries(t *testing.T, r reply) []map[string]any {
	t.Helper()
	data, _ := r.body["data"].([]any)
	var out []map[string]any
	for _, d := range data {
		out = append(out, d.(map[string]any))
	}
	return out
}

func TestTheAuditLogRecordsWhoDidWhat(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	a := h.app("a")
	rc := newReceiver(t)
	tok, tokID := h.token(a, "portal")

	ep := h.do("POST", "/v1/apps/"+a+"/endpoints", map[string]any{"url": rc.srv.URL}, bearer(tok)...)
	epID, secret := str(ep.body, "id"), str(ep.body, "secret")
	base := "/v1/apps/" + a + "/endpoints/" + epID
	h.do("PATCH", base, map[string]any{"description": "d", "concurrency": 2}, bearer(tok)...)
	h.do("GET", base+"/secret", nil, bearer(tok)...)
	rot := h.do("POST", base+"/secret/rotate", nil)
	h.do("POST", base+"/recover", nil)
	h.do("DELETE", base, nil)
	h.do("DELETE", "/v1/apps/"+a+"/tokens/"+tokID, nil)

	log := entries(t, h.do("GET", "/v1/apps/"+a+"/audit-log", nil))
	var actions []string
	for _, e := range log { // newest first
		actions = append([]string{str(e, "action")}, actions...)
	}
	want := "app.create token.create endpoint.create endpoint.update endpoint.secret_read endpoint.secret_rotate endpoint.recover endpoint.delete token.revoke"
	if strings.Join(actions, " ") != want {
		t.Fatalf("actions\n got %v\nwant %s", actions, want)
	}
	byAction := map[string]map[string]any{}
	for _, e := range log {
		byAction[str(e, "action")] = e
	}
	if e := byAction["endpoint.create"]; str(e, "actor_kind") != "app_token" || str(e, "actor_id") != tokID || str(e, "resource_id") != epID || str(e, "app_id") == "" || str(e, "request_id") == "" {
		t.Fatalf("an action by a token names the token: %v", e)
	}
	if e := byAction["endpoint.delete"]; str(e, "actor_kind") != "admin" || str(e, "actor_id") != "admin" {
		t.Fatalf("an action by the operator says so: %v", e)
	}
	if e := byAction["endpoint.update"]; fmt.Sprint(e["detail"]) != "map[fields:[concurrency description]]" {
		t.Fatalf("the audit log names what changed, not the values: %v", e["detail"])
	}
	raw := string(h.do("GET", "/v1/audit-log", nil).raw)
	for _, secretText := range []string{secret, str(rot.body, "secret"), tok[10:]} {
		if strings.Contains(raw, secretText) {
			t.Fatalf("a secret or token appeared in the audit log: %s", raw)
		}
	}
}

func TestAnAuditEntryIsMadeOnlyWhenTheActionWorked(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	a := h.app("a")
	h.do("POST", "/v1/apps/"+a+"/endpoints", map[string]any{"url": "ftp://nope"})                       // refused
	h.do("PATCH", "/v1/apps/"+a+"/endpoints/ep_aaaaaaaaaaaaaaaaaaaa", map[string]any{"concurrency": 2}) // no such endpoint
	h.do("DELETE", "/v1/apps/"+a+"/endpoints/ep_aaaaaaaaaaaaaaaaaaaa", nil)
	if log := entries(t, h.do("GET", "/v1/apps/"+a+"/audit-log", nil)); len(log) != 1 || str(log[0], "action") != "app.create" {
		t.Fatalf("failed actions leave no entry: %v", log)
	}
}

func TestAuditLogReadingIsScopedAndFilterable(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	a, b := h.app("a"), h.app("b")
	tok, _ := h.token(a, "portal")
	h.token(b, "other")

	own := entries(t, h.do("GET", "/v1/apps/"+a+"/audit-log", nil, bearer(tok)...))
	for _, e := range own {
		if str(e, "app_id") != a {
			t.Fatalf("a token sees only its own app's entries: %v", e)
		}
	}
	if len(own) != 2 {
		t.Fatalf("app.create and token.create: %v", own)
	}
	if r := h.do("GET", "/v1/apps/"+b+"/audit-log", nil, bearer(tok)...); r.status != 404 {
		t.Fatalf("another app's log: %d", r.status)
	}
	if r := h.do("GET", "/v1/audit-log", nil, bearer(tok)...); r.status != 403 {
		t.Fatalf("the global log is the operator's: %d", r.status)
	}
	all := entries(t, h.do("GET", "/v1/audit-log", nil))
	if len(all) != 4 {
		t.Fatalf("the operator sees both apps: %d", len(all))
	}
	if got := entries(t, h.do("GET", "/v1/audit-log?app="+b, nil)); len(got) != 2 {
		t.Fatalf("filter by app: %d", len(got))
	}
	if got := entries(t, h.do("GET", "/v1/audit-log?action=token.create", nil)); len(got) != 2 {
		t.Fatalf("filter by action: %d", len(got))
	}
	if r := h.do("GET", "/v1/audit-log?app=ghost", nil); r.status != 404 {
		t.Fatalf("an unknown app: %d", r.status)
	}
	for _, bad := range []string{"?limit=0", "?limit=999", "?cursor=!!", "?action=" + strings.Repeat("a", 65)} {
		if r := h.do("GET", "/v1/audit-log"+bad, nil); r.status != 400 {
			t.Errorf("%s: %d", bad, r.status)
		}
	}
	// Paging.
	p1 := h.do("GET", "/v1/audit-log?limit=3", nil)
	next := str(p1.body, "next_cursor")
	p2 := h.do("GET", "/v1/audit-log?limit=3&cursor="+next, nil)
	if len(entries(t, p1)) != 3 || next == "" || len(entries(t, p2)) != 1 {
		t.Fatalf("paging: %s / %s", p1.raw, p2.raw)
	}
}

// breakAuditLog makes every write to the audit log fail, through a second connection to the same file, the way a full
// disk or a broken table would.
func (h *harness) breakAuditLog() {
	h.t.Helper()
	db, err := sql.Open("sqlite", "file:"+h.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		h.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER audit_down BEFORE INSERT ON audit_log BEGIN SELECT RAISE(ABORT, 'audit log unavailable'); END`); err != nil {
		h.t.Fatal(err)
	}
}

func TestAChangeIsRefusedAndCountedWhenItsAuditEntryCannotBeWritten(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	a := h.app("a")
	h.breakAuditLog()
	rc := newReceiver(t)
	r := h.do("POST", "/v1/apps/"+a+"/endpoints", map[string]any{"url": rc.srv.URL})
	if r.status != 500 || !strings.Contains(string(r.raw), "audit log") || strings.Contains(string(r.raw), "audit log unavailable") {
		t.Fatalf("the caller is told the change was not made, without the database's words: %d %s", r.status, r.raw)
	}
	if got := h.do("GET", "/v1/apps/"+a+"/endpoints", nil); len(got.body["data"].([]any)) != 0 {
		t.Fatalf("the endpoint exists although the request failed: %s", got.raw)
	}
	if text := string(h.do("GET", "/metrics", nil).raw); !strings.Contains(text, "bowler_audit_errors_total 1") {
		t.Fatalf("a broken audit log must show in the metrics:\n%s", text)
	}
}

func TestEveryAuditedRouteRefusesWhenTheAuditLogIsBroken(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	a := h.app("a")
	rc := newReceiver(t)
	ep, _ := h.endpoint(a, rc.srv.URL, nil)
	_, tok := h.token(a, "portal")
	base := "/v1/apps/" + a + "/endpoints/" + ep
	h.breakAuditLog()
	for _, c := range []struct{ name, method, path string }{
		{"create an app", "POST", "/v1/apps"},
		{"create an endpoint", "POST", "/v1/apps/" + a + "/endpoints"},
		{"update an endpoint", "PATCH", base},
		{"delete an endpoint", "DELETE", base},
		{"read a secret", "GET", base + "/secret"},
		{"rotate a secret", "POST", base + "/secret/rotate"},
		{"recover an endpoint", "POST", base + "/recover"},
		{"create a token", "POST", "/v1/apps/" + a + "/tokens"},
		{"revoke a token", "DELETE", "/v1/apps/" + a + "/tokens/" + tok},
	} {
		var body any
		switch c.name {
		case "create an app":
			body = map[string]any{"name": "b"}
		case "create an endpoint":
			body = map[string]any{"url": rc.srv.URL}
		case "update an endpoint":
			body = map[string]any{"description": "x"}
		case "create a token":
			body = map[string]any{"name": "second"}
		}
		if r := h.do(c.method, c.path, body); r.status != 500 {
			t.Errorf("%s: want 500, got %d %s", c.name, r.status, r.raw)
		}
	}
	// The endpoint and the token are as they were.
	if got := h.do("GET", "/v1/apps/"+a+"/endpoints", nil); len(got.body["data"].([]any)) != 1 {
		t.Fatalf("the delete went through: %s", got.raw)
	}
	if tk := h.do("GET", "/v1/apps/"+a+"/tokens", nil); tk.status != 200 || strings.Contains(string(tk.raw), "revoked_at") {
		t.Fatalf("the revoke went through: %d %s", tk.status, tk.raw)
	}
}

func TestASecretIsNotGivenOutWhenItsReadCannotBeRecorded(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	a := h.app("a")
	rc := newReceiver(t)
	ep, secret := h.endpoint(a, rc.srv.URL, nil)
	h.breakAuditLog()
	r := h.do("GET", "/v1/apps/"+a+"/endpoints/"+ep+"/secret", nil)
	if r.status != 500 || strings.Contains(string(r.raw), secret) {
		t.Fatalf("no record, no secret: %d %s", r.status, r.raw)
	}
}

func TestReadsAndMessagesStillWorkWhenTheAuditLogIsBroken(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	a := h.app("a")
	rc := newReceiver(t)
	h.endpoint(a, rc.srv.URL, nil)
	h.breakAuditLog()
	if r := h.send(a, "x.y", map[string]any{"n": 1}); r.status != 202 {
		t.Fatalf("sending a message is not audited and must not stop: %d %s", r.status, r.raw)
	}
	if r := h.do("GET", "/v1/apps/"+a+"/endpoints", nil); r.status != 200 {
		t.Fatalf("a list: %d", r.status)
	}
	if r := h.do("GET", "/healthz", nil); r.status != 200 {
		t.Fatalf("health: %d", r.status)
	}
}

func TestMetricsAreForTheOperatorOnly(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	a := h.app("a")
	tok, _ := h.token(a, "portal")
	if r := h.do("GET", "/metrics", nil, bearer(tok)...); r.status != 403 {
		t.Fatalf("%d", r.status)
	}
	if r := h.do("GET", "/metrics", nil, "Authorization", ""); r.status != 401 {
		t.Fatalf("%d", r.status)
	}
	_ = http.StatusOK
}

func (h *harness) tokenWith(app string, body map[string]any) (secret, id string) {
	h.t.Helper()
	r := h.do("POST", "/v1/apps/"+app+"/tokens", body)
	if r.status != 201 {
		h.t.Fatalf("making a token: %d %s", r.status, r.raw)
	}
	return str(r.body, "token"), str(r.body, "id")
}

func TestAReadOnlyTokenLooksAndChangesNothing(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	app := h.app("a")
	rc := newReceiver(t)
	ep, _ := h.endpoint(app, rc.srv.URL, nil)
	tok, _ := h.tokenWith(app, map[string]any{"name": "viewer", "scope": "read"})
	base := "/v1/apps/" + app + "/endpoints/" + ep

	for _, path := range []string{"/v1/apps/" + app, "/v1/apps/" + app + "/endpoints", base, "/v1/apps/" + app + "/messages", "/v1/apps/" + app + "/audit-log"} {
		if r := h.do("GET", path, nil, bearer(tok)...); r.status != 200 {
			t.Errorf("GET %s: a read-only token may look, got %d", path, r.status)
		}
	}
	for name, req := range map[string][3]any{
		"create endpoint": {"POST", "/v1/apps/" + app + "/endpoints", map[string]any{"url": rc.srv.URL}},
		"update endpoint": {"PATCH", base, map[string]any{"description": "x"}},
		"delete endpoint": {"DELETE", base, nil},
		"rotate":          {"POST", base + "/secret/rotate", nil},
		"recover":         {"POST", base + "/recover", nil},
		"send":            {"POST", "/v1/apps/" + app + "/messages", map[string]any{"event_type": "x", "payload": map[string]any{}}},
	} {
		r := h.do(req[0].(string), req[1].(string), req[2], bearer(tok)...)
		if r.status != 403 || str(r.body["error"].(map[string]any), "code") != "read_only" {
			t.Errorf("%s: a read-only token must not change anything, got %d %s", name, r.status, r.raw)
		}
	}
	// Reading the signing secret is a GET, and it is the power to forge webhooks, so it is refused as well.
	if r := h.do("GET", base+"/secret", nil, bearer(tok)...); r.status != 403 || strings.Contains(string(r.raw), "whsec_") {
		t.Fatalf("a read-only token must not read a signing secret: %d %s", r.status, r.raw)
	}
	// And none of it happened.
	if r := h.do("GET", base, nil); str(r.body, "description") != "" {
		t.Fatalf("the endpoint was changed: %s", r.raw)
	}
	if log := entries(t, h.do("GET", "/v1/apps/"+app+"/audit-log?action=endpoint.secret_read", nil)); len(log) != 0 {
		t.Fatalf("a refused secret read leaves no entry: %v", log)
	}
}

// Walk the routes so that a route added later cannot forget that a read-only token may not change things.
func TestNoRouteLetsAReadOnlyTokenChangeAnything(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	app := h.app("a")
	rc := newReceiver(t)
	ep, _ := h.endpoint(app, rc.srv.URL, nil)
	tok, _ := h.tokenWith(app, map[string]any{"name": "viewer", "scope": "read"})
	fill := strings.NewReplacer("{app}", app, "{endpoint}", ep, "{message}", "msg_aaaaaaaaaaaaaaaaaaaa",
		"{delivery}", "dlv_aaaaaaaaaaaaaaaaaaaa", "{token}", "tok_aaaaaaaaaaaaaaaaaaaa")
	checked := 0
	for _, route := range h.api.Routes() {
		method, path, _ := strings.Cut(route, " ")
		if path == "/healthz" || path == "/readyz" || !strings.Contains(path, "{app}") {
			continue
		}
		var body any
		if method == "POST" || method == "PATCH" {
			body = map[string]any{}
		}
		r := h.do(method, fill.Replace(path), body, bearer(tok)...)
		switch {
		case method != "GET":
			if r.status != 403 {
				t.Errorf("%s with a read-only token answered %d: %s", route, r.status, r.raw)
			}
		case strings.HasSuffix(path, "/secret"):
			if r.status != 403 {
				t.Errorf("%s with a read-only token answered %d: %s", route, r.status, r.raw)
			}
		case strings.HasSuffix(path, "/tokens"):
			if r.status != 403 {
				t.Errorf("%s is the operator's: %d", route, r.status)
			}
		default:
			if r.status == 403 || r.status == 401 {
				t.Errorf("%s: a read-only token may look, got %d: %s", route, r.status, r.raw)
			}
		}
		checked++
	}
	if checked < 15 {
		t.Fatalf("only %d routes were checked", checked)
	}
}

func TestATokenCanBeMadeToExpire(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	app := h.app("a")
	now := time.Now()
	h.api.now = func() time.Time { return now }

	r := h.do("POST", "/v1/apps/"+app+"/tokens", map[string]any{"name": "short", "expires_in_hours": 2})
	tok := str(r.body, "token")
	if r.status != 201 || str(r.body, "expires_at") == "" || str(r.body, "scope") != "full" {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	if got := h.do("GET", "/v1/apps/"+app, nil, bearer(tok)...); got.status != 200 {
		t.Fatalf("it works at first: %d", got.status)
	}
	now = now.Add(119 * time.Minute)
	if got := h.do("GET", "/v1/apps/"+app, nil, bearer(tok)...); got.status != 200 {
		t.Fatalf("and until the last minute: %d", got.status)
	}
	now = now.Add(2 * time.Minute)
	if got := h.do("GET", "/v1/apps/"+app, nil, bearer(tok)...); got.status != 401 {
		t.Fatalf("an expired token is refused like a wrong one: %d", got.status)
	}
	list := h.do("GET", "/v1/apps/"+app+"/tokens", nil)
	if data, _ := list.body["data"].([]any); len(data) != 1 || data[0].(map[string]any)["expires_at"] == nil {
		t.Fatalf("an expired token stays in the list: %s", list.raw)
	}
}

func TestTokenScopeAndExpiryAreValidated(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	app := h.app("a")
	for name, body := range map[string]map[string]any{
		"unknown scope":    {"name": "x", "scope": "admin"},
		"scope wrong case": {"name": "x", "scope": "READ"},
		"zero hours":       {"name": "x", "expires_in_hours": 0},
		"negative hours":   {"name": "x", "expires_in_hours": -5},
		"over a year":      {"name": "x", "expires_in_hours": 24*365 + 1},
		"hours as text":    {"name": "x", "expires_in_hours": "5"},
	} {
		if r := h.do("POST", "/v1/apps/"+app+"/tokens", body); r.status != 400 {
			t.Errorf("%s: got %d", name, r.status)
		}
	}
	if r := h.do("POST", "/v1/apps/"+app+"/tokens", map[string]any{"name": "ok", "scope": "read", "expires_in_hours": 24 * 365}); r.status != 201 {
		t.Errorf("a year and read scope are fine: %d %s", r.status, r.raw)
	}
}

func TestTheUseOfATokenIsRecordedRoughly(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	app := h.app("a")
	now := time.Now().Truncate(time.Second)
	h.api.now = func() time.Time { return now }
	tok, id := h.token(app, "portal")
	lastUsed := func() string {
		list := h.do("GET", "/v1/apps/"+app+"/tokens", nil)
		for _, d := range list.body["data"].([]any) {
			if m := d.(map[string]any); str(m, "id") == id {
				return str(m, "last_used_at")
			}
		}
		return "?"
	}
	if got := lastUsed(); got != "" {
		t.Fatalf("a token that was never used has no last use: %q", got)
	}
	h.do("GET", "/v1/apps/"+app, nil, bearer(tok)...)
	first := lastUsed()
	if first == "" {
		t.Fatal("the first use is recorded")
	}
	now = now.Add(5 * time.Minute)
	h.do("GET", "/v1/apps/"+app, nil, bearer(tok)...)
	if got := lastUsed(); got != first {
		t.Fatalf("within ten minutes it is not written again, so use does not become a write each time: %q then %q", first, got)
	}
	now = now.Add(6 * time.Minute)
	h.do("GET", "/v1/apps/"+app, nil, bearer(tok)...)
	if got := lastUsed(); got == first {
		t.Fatal("after ten minutes it is updated")
	}
}

func TestTheAuditLogSaysWhatKindOfTokenWasMade(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	app := h.app("a")
	h.tokenWith(app, map[string]any{"name": "viewer", "scope": "read", "expires_in_hours": 5})
	log := entries(t, h.do("GET", "/v1/apps/"+app+"/audit-log?action=token.create", nil))
	if len(log) != 1 || fmt.Sprint(log[0]["detail"]) == "" || !strings.Contains(fmt.Sprint(log[0]["detail"]), "scope:read") || !strings.Contains(fmt.Sprint(log[0]["detail"]), "expires_at") {
		t.Fatalf("%v", log)
	}
}
