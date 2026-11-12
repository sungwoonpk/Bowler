package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sungwoonpk/bowler/internal/domain"
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

func TestAFailureToRecordAnAuditEntryDoesNotFailTheRequestButIsCounted(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.noWorker = true })
	a := h.app("a")
	h.api.recordAudit = func(context.Context, domain.AuditEntry, time.Time) (domain.AuditEntry, error) {
		return domain.AuditEntry{}, errors.New("disk full")
	}
	rc := newReceiver(t)
	if r := h.do("POST", "/v1/apps/"+a+"/endpoints", map[string]any{"url": rc.srv.URL}); r.status != 201 {
		t.Fatalf("the action happened, so the caller is told so: %d %s", r.status, r.raw)
	}
	if text := string(h.do("GET", "/metrics", nil).raw); !strings.Contains(text, "bowler_audit_errors_total 1") {
		t.Fatalf("a gap in the record must show in the metrics:\n%s", text)
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
