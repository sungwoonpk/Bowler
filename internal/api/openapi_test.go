package api

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/sungwoonpk/bowler/signature"
)

// These tests keep docs/openapi.json true. The file is what a customer reads, so a route that exists but is not
// described, or is described but does not exist, is a bug.

type spec struct {
	Paths   map[string]map[string]operation `json:"paths"`
	Webhook map[string]map[string]operation `json:"webhooks"`
	Comp    struct {
		Parameters map[string]json.RawMessage `json:"parameters"`
		Schemas    map[string]json.RawMessage `json:"schemas"`
	} `json:"components"`
}

type operation struct {
	OperationID string            `json:"operationId"`
	Security    *[]map[string]any `json:"security"`
	Parameters  []parameter       `json:"parameters"`
}

type parameter struct {
	Ref  string `json:"$ref"`
	Name string `json:"name"`
	In   string `json:"in"`
}

func loadSpec(t *testing.T) (spec, []byte) {
	t.Helper()
	raw, err := os.ReadFile("../../docs/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var s spec
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("docs/openapi.json is not valid JSON: %v", err)
	}
	return s, raw
}

func documented(s spec) []string {
	var out []string
	for path, item := range s.Paths {
		for method := range item {
			out = append(out, strings.ToUpper(method)+" "+path)
		}
	}
	sort.Strings(out)
	return out
}

func TestTheSpecDescribesExactlyTheRoutesTheServerHas(t *testing.T) {
	s, _ := loadSpec(t)
	h := newHarness(t, func(c *setup) { c.noWorker = true })
	have, want := h.api.Routes(), documented(s)
	missing, extra := diff(want, have), diff(have, want)
	if len(missing) > 0 {
		t.Errorf("documented but the server has no such route: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("the server has these routes but docs/openapi.json does not describe them: %v", extra)
	}
}

func diff(a, b []string) []string {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []string
	for _, x := range a {
		if !in[x] {
			out = append(out, x)
		}
	}
	return out
}

func TestSecurityInTheSpecMatchesWhatTheServerEnforces(t *testing.T) {
	s, _ := loadSpec(t)
	h := newHarness(t, func(c *setup) { c.noWorker = true })
	for path, item := range s.Paths {
		for method, op := range item {
			public := op.Security != nil && len(*op.Security) == 0
			url := h.srv.URL + strings.NewReplacer("{app}", "x", "{endpoint}", "ep_aaaaaaaaaaaaaaaaaaaa", "{message}", "msg_aaaaaaaaaaaaaaaaaaaa", "{delivery}", "dlv_aaaaaaaaaaaaaaaaaaaa").Replace(path)
			req, _ := http.NewRequest(strings.ToUpper(method), url, nil) // no key
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			label := strings.ToUpper(method) + " " + path
			switch {
			case public && resp.StatusCode == http.StatusUnauthorized:
				t.Errorf("%s is documented as public but asks for a key", label)
			case !public && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusTooManyRequests:
				t.Errorf("%s is documented as needing a key but answered %d without one", label, resp.StatusCode)
			}
		}
	}
}

func TestEveryReferenceInTheSpecResolves(t *testing.T) {
	_, raw := loadSpec(t)
	var doc any
	_ = json.Unmarshal(raw, &doc)
	var refs []string
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			for k, x := range v {
				if k == "$ref" {
					refs = append(refs, x.(string))
				} else {
					walk(x)
				}
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(doc)
	if len(refs) < 20 {
		t.Fatalf("expected the spec to use references, found %d", len(refs))
	}
	for _, ref := range refs {
		target := doc
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, ok := target.(map[string]any)
			if !ok {
				target = nil
				break
			}
			target = m[part]
		}
		if target == nil {
			t.Errorf("%s does not resolve", ref)
		}
	}
}

func TestEveryPathParameterIsDeclaredAndOperationIDsAreUnique(t *testing.T) {
	s, _ := loadSpec(t)
	seen := map[string]string{}
	for path, item := range s.Paths {
		for method, op := range item {
			label := strings.ToUpper(method) + " " + path
			if op.OperationID == "" {
				t.Errorf("%s has no operationId", label)
			} else if prev, dup := seen[op.OperationID]; dup {
				t.Errorf("operationId %q is used by both %s and %s", op.OperationID, prev, label)
			}
			seen[op.OperationID] = label

			declared := map[string]bool{}
			for _, p := range op.Parameters {
				name := p.Name
				if p.Ref != "" {
					name = strings.TrimPrefix(p.Ref, "#/components/parameters/")
				}
				declared[name] = true
			}
			for _, seg := range strings.Split(path, "/") {
				if strings.HasPrefix(seg, "{") {
					if name := strings.Trim(seg, "{}"); !declared[name] {
						t.Errorf("%s uses {%s} but does not declare it", label, name)
					}
				}
			}
		}
	}
}

func TestTheDocumentedWebhookHeadersAreTheOnesThatAreSent(t *testing.T) {
	s, _ := loadSpec(t)
	op := s.Webhook["delivery"]["post"]
	got := map[string]bool{}
	for _, p := range op.Parameters {
		if p.In == "header" {
			got[p.Name] = true
		}
	}
	for _, want := range []string{signature.HeaderID, signature.HeaderTimestamp, signature.HeaderSignature} {
		if !got[want] {
			t.Errorf("the webhook the customer receives carries %q, but the spec does not say so", want)
		}
	}
	if len(got) != 3 {
		t.Errorf("the spec lists headers the code does not send: %v", got)
	}
}
