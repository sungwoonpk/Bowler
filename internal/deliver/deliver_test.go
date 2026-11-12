package deliver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/ssrf"
	"github.com/sungwoonpk/bowler/internal/store"
	"github.com/sungwoonpk/bowler/signature"
)

const testSecret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"

// drain reads the request body. Go's server only starts watching for a client that has gone away once the body has
// been read, so a handler that waits on r.Context() must do this first or it waits forever.
func drain(r *http.Request) { _, _ = io.Copy(io.Discard, r.Body) }

func claim(url, payload string) store.Claim {
	return store.Claim{
		Delivery: domain.Delivery{ID: "dlv_test", AttemptCount: 1},
		Endpoint: domain.Endpoint{ID: "ep_test", URL: url, Secret: testSecret},
		Message:  domain.Message{ID: "msg_abc123", EventType: "invoice.paid", Payload: []byte(payload)},
	}
}

// local trusts loopback, since the receivers in these tests run on this machine.
func local() *Sender {
	return New(Config{SSRF: ssrf.Policy{AllowPrivate: true}, Timeout: 2 * time.Second})
}

func TestSuccessIsSignedAndVerifiable(t *testing.T) {
	var got struct {
		method, contentType, agent, id, ts, sig string
		body                                    []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.contentType, got.agent = r.Method, r.Header.Get("Content-Type"), r.Header.Get("User-Agent")
		got.id, got.ts, got.sig = r.Header.Get("webhook-id"), r.Header.Get("webhook-timestamp"), r.Header.Get("webhook-signature")
		got.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(204)
	}))
	defer srv.Close()

	payload := `{"invoice":"in_1","amount":4200,"note":"café ✓"}`
	out := New(Config{SSRF: ssrf.Policy{AllowPrivate: true}, UserAgent: "Bowler/test"}).Send(context.Background(), claim(srv.URL, payload))
	if out.Verdict != Success || out.Attempt.StatusCode != 204 || out.Attempt.Failure != "" {
		t.Fatalf("%+v", out)
	}
	if got.method != "POST" || got.contentType != "application/json" || got.agent != "Bowler/test" {
		t.Fatalf("request line: %+v", got)
	}
	if string(got.body) != payload {
		t.Fatalf("the body must arrive byte for byte, got %q", got.body)
	}
	if got.id != "msg_abc123" {
		t.Fatalf("webhook-id must be the message id, got %q", got.id)
	}
	// What a customer's server would do with the three headers.
	if err := signature.Verify([]string{testSecret}, got.id, got.ts, got.sig, got.body, time.Now(), 0); err != nil {
		t.Fatalf("the receiver could not verify the signature: %v", err)
	}
	if out.Attempt.Trigger != domain.Scheduled || out.Attempt.DeliveryID != "dlv_test" {
		t.Fatalf("attempt bookkeeping: %+v", out.Attempt)
	}
}

func TestRetriesKeepTheSameIDButFreshSignatures(t *testing.T) {
	var ids, sigs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids = append(ids, r.Header.Get("webhook-id"))
		sigs = append(sigs, r.Header.Get("webhook-timestamp"))
		w.WriteHeader(500)
	}))
	defer srv.Close()
	clock := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	s := New(Config{SSRF: ssrf.Policy{AllowPrivate: true}, Now: func() time.Time { clock = clock.Add(time.Minute); return clock }})
	s.Send(context.Background(), claim(srv.URL, `{}`))
	s.Send(context.Background(), claim(srv.URL, `{}`))
	if ids[0] != ids[1] {
		t.Fatalf("the id must not change between attempts, or the receiver cannot drop duplicates: %v", ids)
	}
	if sigs[0] == sigs[1] {
		t.Fatalf("each attempt carries its own timestamp: %v", sigs)
	}
}

func TestStatusCodesBecomeVerdicts(t *testing.T) {
	cases := []struct {
		status  int
		verdict Verdict
		failure domain.Failure
		disable bool
	}{
		{200, Success, "", false}, {201, Success, "", false}, {204, Success, "", false}, {299, Success, "", false},
		{301, Retry, domain.FailRedirect, false}, {302, Retry, domain.FailRedirect, false}, {307, Retry, domain.FailRedirect, false},
		{400, Retry, domain.FailClientError, false}, {401, Retry, domain.FailClientError, false}, {404, Retry, domain.FailClientError, false}, {422, Retry, domain.FailClientError, false},
		{410, Fail, domain.FailGone, true},
		{429, Retry, domain.FailRateLimited, false},
		{500, Retry, domain.FailServerError, false}, {502, Retry, domain.FailServerError, false}, {503, Retry, domain.FailServerError, false}, {504, Retry, domain.FailServerError, false},
	}
	for _, c := range cases {
		t.Run(strconv.Itoa(c.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.status >= 300 && c.status < 400 {
					w.Header().Set("Location", "https://example.com/elsewhere")
				}
				w.WriteHeader(c.status)
			}))
			defer srv.Close()
			out := local().Send(context.Background(), claim(srv.URL, `{}`))
			if out.Verdict != c.verdict || out.Attempt.Failure != c.failure || (out.Disable != "") != c.disable || out.Attempt.StatusCode != c.status {
				t.Fatalf("status %d: verdict=%v failure=%q disable=%q", c.status, out.Verdict, out.Attempt.Failure, out.Disable)
			}
		})
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	out := local().Send(context.Background(), claim(redirector.URL, `{}`))
	if out.Verdict != Retry || out.Attempt.Failure != domain.FailRedirect {
		t.Fatalf("a redirect is a failure: %+v", out)
	}
	if hits.Load() != 0 {
		t.Fatal("the redirect target must never be called")
	}
}

func TestRetryAfterIsPassedOn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "90")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	if out := local().Send(context.Background(), claim(srv.URL, `{}`)); out.RetryAfter != 90*time.Second {
		t.Fatalf("RetryAfter = %v", out.RetryAfter)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":     0,
		"30":   30 * time.Second,
		" 5 ":  5 * time.Second,
		"0":    0,
		"-5":   0,
		"soon": 0,
		now.Add(2 * time.Minute).Format(http.TimeFormat): 2 * time.Minute,
		now.Add(-time.Hour).Format(http.TimeFormat):      0,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSlowReceiverTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drain(r)
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	started := time.Now()
	out := New(Config{SSRF: ssrf.Policy{AllowPrivate: true}, Timeout: 150 * time.Millisecond}).Send(context.Background(), claim(srv.URL, `{}`))
	if out.Verdict != Retry || out.Attempt.Failure != domain.FailTimeout {
		t.Fatalf("%+v", out)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("the timeout was not honored, took %v", time.Since(started))
	}
}

func TestConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	out := local().Send(context.Background(), claim(url, `{}`))
	if out.Verdict != Retry || out.Attempt.Failure != domain.FailRefused {
		t.Fatalf("%+v", out)
	}
}

func TestUnknownHostIsADNSFailure(t *testing.T) {
	out := New(Config{Timeout: 5 * time.Second}).Send(context.Background(), claim("https://no-such-host.invalid/hook", `{}`))
	if out.Verdict != Retry || out.Attempt.Failure != domain.FailDNS {
		t.Fatalf("%+v", out)
	}
}

func TestUntrustedCertificateIsATLSFailure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	out := local().Send(context.Background(), claim(srv.URL, `{}`))
	if out.Verdict != Retry || out.Attempt.Failure != domain.FailTLS {
		t.Fatalf("a self-signed certificate must be refused: %+v", out)
	}
}

func TestTrustedCertificateWorks(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	s := New(Config{SSRF: ssrf.Policy{AllowPrivate: true}, TLS: &tls.Config{RootCAs: pool}})
	if out := s.Send(context.Background(), claim(srv.URL, `{}`)); out.Verdict != Success {
		t.Fatalf("%+v", out)
	}
}

func TestPrivateAddressesAreNeverCalled(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(200) }))
	defer srv.Close()
	out := New(Config{}).Send(context.Background(), claim(srv.URL, `{}`)) // the default policy
	if out.Verdict != Fail || out.Attempt.Failure != domain.FailBlocked || out.Disable == "" {
		t.Fatalf("a blocked address is a permanent failure that switches the endpoint off: %+v", out)
	}
	if hits.Load() != 0 {
		t.Fatal("the server must never have been reached")
	}
}

func TestEnvironmentProxyIsIgnored(t *testing.T) {
	// A proxy in the environment must not be used, because it would hide the real destination from the address check.
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxied.Add(1); w.WriteHeader(200) }))
	defer proxy.Close()
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer direct.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")

	out := local().Send(context.Background(), claim(direct.URL, `{}`))
	if out.Verdict != Success || proxied.Load() != 0 {
		t.Fatalf("the request must go straight to the receiver: verdict=%v proxied=%d", out.Verdict, proxied.Load())
	}
}

func TestShutdownAbortsInsteadOfFailing(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drain(r)
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Outcome, 1)
	go func() { done <- local().Send(ctx, claim(srv.URL, `{}`)) }()
	<-started
	cancel()
	select {
	case out := <-done:
		if out.Verdict != Abort {
			t.Fatalf("cancelling the caller is an abort, not a receiver failure: %+v", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not return after cancellation")
	}
}

func TestResponseSnippetIsBoundedAndCleaned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, strings.Repeat("x", 5000))
	}))
	defer srv.Close()
	out := local().Send(context.Background(), claim(srv.URL, `{}`))
	if len(out.Attempt.ResponseSnippet) != 1024 {
		t.Fatalf("the snippet is cut to 1024 bytes, got %d", len(out.Attempt.ResponseSnippet))
	}

	dirty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("bad\x00bytes \xff\xfe and\x1b[31m escapes\nsecond line\ttabbed"))
	}))
	defer dirty.Close()
	got := local().Send(context.Background(), claim(dirty.URL, `{}`)).Attempt.ResponseSnippet
	if strings.ContainsAny(got, "\x00\x1b") || !strings.Contains(got, "second line\ttabbed") {
		t.Fatalf("control characters must go and ordinary whitespace stay: %q", got)
	}
}

func TestEndlessResponseDoesNotHoldTheWorker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drain(r)
		w.WriteHeader(200)
		chunk := []byte(strings.Repeat("y", 32<<10))
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	started := time.Now()
	out := local().Send(context.Background(), claim(srv.URL, `{}`))
	if out.Verdict != Success {
		t.Fatalf("a 200 is a success however much follows: %+v", out)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("reading must stop at a bound, took %v", time.Since(started))
	}
}

func TestManualAttemptsAreMarked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	c := claim(srv.URL, `{}`)
	c.Manual = true
	if out := local().Send(context.Background(), c); out.Attempt.Trigger != domain.Manual {
		t.Fatalf("trigger = %q", out.Attempt.Trigger)
	}
}

func TestDuringRotationBothSecretsSign(t *testing.T) {
	newSecret, _ := signature.GenerateSecret()
	var ts, sig string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts, sig = r.Header.Get("webhook-timestamp"), r.Header.Get("webhook-signature")
		body, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	c := claim(srv.URL, `{"a":1}`)
	grace := time.Now().Add(time.Hour)
	c.Endpoint.Secret, c.Endpoint.PrevSecret, c.Endpoint.PrevSecretExpiresAt = newSecret, testSecret, &grace
	local().Send(context.Background(), c)

	if n := len(strings.Fields(sig)); n != 2 {
		t.Fatalf("expected two signatures during rotation, got %d in %q", n, sig)
	}
	for name, secret := range map[string]string{"old": testSecret, "new": newSecret} {
		if err := signature.Verify([]string{secret}, "msg_abc123", ts, sig, body, time.Now(), 0); err != nil {
			t.Errorf("a receiver on the %s secret must accept it: %v", name, err)
		}
	}
}

func TestUnusableSecretFailsPermanently(t *testing.T) {
	c := claim("http://127.0.0.1:1/", `{}`)
	c.Endpoint.Secret = "whsec_***"
	out := local().Send(context.Background(), c)
	if out.Verdict != Fail {
		t.Fatalf("a secret that cannot sign cannot be fixed by retrying: %+v", out)
	}
}

func TestClassifyErrorCoversTheCommonCases(t *testing.T) {
	cases := map[string]struct {
		err  error
		want domain.Failure
	}{
		"deadline": {context.DeadlineExceeded, domain.FailTimeout},
		"blocked":  {fmt.Errorf("dial: %w", ssrf.ErrBlocked), domain.FailBlocked},
		"dns":      {&net.DNSError{Err: "no such host", Name: "x"}, domain.FailDNS},
		"other":    {fmt.Errorf("something odd"), domain.FailConnection},
	}
	for name, c := range cases {
		if got := classifyError(c.err); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

func TestShortenNeverCutsACharacterInHalf(t *testing.T) {
	if got := shorten("abcdef", 10); got != "abcdef" {
		t.Errorf("a short text is left alone: %q", got)
	}
	// "é" is two bytes. Cutting after the first byte would leave an invalid character.
	got := shorten("aé", 2)
	if got != "a" || !utf8Valid(got) {
		t.Errorf("a cut in the middle of a character drops that character: %q", got)
	}
	if got := shorten("日本語日本語", 7); !utf8Valid(got) || len(got) > 7 {
		t.Errorf("%q", got)
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "") == s }

func TestCloseIsSafeToCallTwice(t *testing.T) {
	s := local()
	s.Close()
	s.Close()
}
