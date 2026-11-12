// Package deliver makes one attempt to send a webhook and says what happened.
//
// It does not decide what to do next. It reports a Verdict, and the dispatcher turns that into a retry, a
// failure or a success in the queue.
package deliver

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/ssrf"
	"github.com/sungwoonpk/bowler/internal/store"
	"github.com/sungwoonpk/bowler/signature"
)

// Verdict is what an attempt means for the delivery.
type Verdict int

const (
	// Success: the receiver answered with a 2xx.
	Success Verdict = iota + 1
	// Retry: it failed in a way that may go away, so try again later.
	Retry
	// Fail: it failed in a way that will not go away by itself, so stop.
	Fail
	// Abort: Bowler was shutting down, so the attempt says nothing about the receiver. Do not count it.
	Abort
)

// Outcome is the result of one attempt.
type Outcome struct {
	Verdict Verdict
	Attempt domain.Attempt
	// RetryAfter is how long the receiver asked us to wait, or zero.
	RetryAfter time.Duration
	// Disable, when not empty, is the reason to switch the endpoint off.
	Disable string
}

// Config controls how requests are made. Zero values get sensible defaults.
type Config struct {
	// Timeout is how long one attempt may take in all. Default 15 seconds.
	Timeout time.Duration
	// SSRF says which addresses may be called. The zero value allows public addresses only.
	SSRF ssrf.Policy
	// UserAgent is sent with every request. Default "Bowler".
	UserAgent string
	// MaxSnippet is how much of a response to keep for the attempt history. Default 1024 bytes.
	MaxSnippet int
	// TLS lets tests trust a test certificate. Leave nil in real use.
	TLS *tls.Config
	// Now is the clock. Default time.Now.
	Now func() time.Time
}

// Sender sends webhooks. It is safe for use from many goroutines and reuses connections.
type Sender struct {
	client *http.Client
	cfg    Config
}

// New builds a Sender.
func New(cfg Config) *Sender {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "Bowler"
	}
	if cfg.MaxSnippet <= 0 {
		cfg.MaxSnippet = 1024
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	transport := &http.Transport{
		// Proxy is left nil on purpose. Following HTTP_PROXY from the environment would send the connection to the
		// proxy instead of the receiver, and the address check below would see only the proxy's address, so it
		// could be used to reach private machines.
		Proxy: nil,
		// Control sees the address the name resolved to, at the moment of connecting. This is the SSRF check
		// that cannot be dodged with DNS tricks.
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: cfg.SSRF.Control}).DialContext,
		TLSClientConfig:       cfg.TLS,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: cfg.Timeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   domain.MaxConcurrency,
		IdleConnTimeout:       60 * time.Second,
	}
	return &Sender{
		cfg: cfg,
		client: &http.Client{
			Transport: transport,
			// A redirect is not followed. A receiver on a public address could otherwise answer "301 to
			// http://169.254.169.254/" and have Bowler walk there. The attempt is reported as a failure instead.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Close drops idle connections.
func (s *Sender) Close() { s.client.CloseIdleConnections() }

// Send makes one attempt for a claimed delivery.
func (s *Sender) Send(parent context.Context, c store.Claim) Outcome {
	started := s.cfg.Now()
	trigger := domain.Scheduled
	if c.Manual {
		trigger = domain.Manual
	}
	out := Outcome{Attempt: domain.Attempt{DeliveryID: c.Delivery.ID, StartedAt: started, Trigger: trigger}}
	finish := func(v Verdict) Outcome {
		out.Verdict = v
		out.Attempt.Duration = s.cfg.Now().Sub(started)
		return out
	}

	body := []byte(c.Message.Payload)
	sig, err := signature.SignAll(c.Endpoint.SigningSecrets(started), c.Message.ID, started, body)
	if err != nil {
		// A stored secret that cannot be used is a fault on our side that no retry will mend.
		out.Attempt.Failure = domain.FailConnection
		out.Attempt.ResponseSnippet = "cannot sign: " + err.Error()
		return finish(Fail)
	}

	ctx, cancel := context.WithTimeout(parent, s.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint.URL, bytes.NewReader(body))
	if err != nil {
		out.Attempt.Failure = domain.FailConnection
		out.Attempt.ResponseSnippet = "bad url: " + err.Error()
		return finish(Fail)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", s.cfg.UserAgent)
	// The id is the message id, the same on every attempt, so the receiver can recognize a repeat and ignore it.
	req.Header.Set(signature.HeaderID, c.Message.ID)
	req.Header.Set(signature.HeaderTimestamp, strconv.FormatInt(started.Unix(), 10))
	req.Header.Set(signature.HeaderSignature, sig)

	resp, err := s.client.Do(req)
	if err != nil {
		if parent.Err() != nil {
			return finish(Abort)
		}
		failure := classifyError(err)
		out.Attempt.Failure = failure
		out.Attempt.ResponseSnippet = shorten(err.Error(), s.cfg.MaxSnippet)
		if failure == domain.FailBlocked {
			out.Disable = "the address is private or reserved, which Bowler does not call"
			return finish(Fail)
		}
		return finish(Retry)
	}
	defer resp.Body.Close()

	out.Attempt.StatusCode = resp.StatusCode
	out.Attempt.ResponseSnippet = s.readSnippet(resp.Body)

	switch code := resp.StatusCode; {
	case code >= 200 && code < 300:
		return finish(Success)
	case code == http.StatusGone:
		out.Attempt.Failure = domain.FailGone
		out.Disable = "the receiver answered 410 Gone"
		return finish(Fail)
	case code == http.StatusTooManyRequests:
		out.Attempt.Failure = domain.FailRateLimited
		out.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), s.cfg.Now())
		return finish(Retry)
	case code >= 300 && code < 400:
		out.Attempt.Failure = domain.FailRedirect
		return finish(Retry)
	case code >= 400 && code < 500:
		out.Attempt.Failure = domain.FailClientError
		return finish(Retry)
	default:
		out.Attempt.Failure = domain.FailServerError
		out.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), s.cfg.Now())
		return finish(Retry)
	}
}

// readSnippet keeps the start of a response for the history and throws the rest away. It reads a bounded amount
// after that so the connection can be reused, but never an unbounded one: a receiver that streams forever must not
// hold a worker.
func (s *Sender) readSnippet(r io.Reader) string {
	head, _ := io.ReadAll(io.LimitReader(r, int64(s.cfg.MaxSnippet)))
	_, _ = io.Copy(io.Discard, io.LimitReader(r, 64<<10))
	return clean(string(head))
}

// clean makes a response safe to store and show: valid text, no control characters.
func clean(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func shorten(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "")
}

// classifyError names a network failure in terms a person can act on.
func classifyError(err error) domain.Failure {
	switch {
	case errors.Is(err, ssrf.ErrBlocked):
		return domain.FailBlocked
	case errors.Is(err, context.DeadlineExceeded):
		return domain.FailTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return domain.FailTimeout
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return domain.FailDNS
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return domain.FailRefused
	}
	var (
		unknownAuthority x509.UnknownAuthorityError
		hostname         x509.HostnameError
		invalid          x509.CertificateInvalidError
		recordHeader     tls.RecordHeaderError
		certVerify       *tls.CertificateVerificationError
	)
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid) ||
		errors.As(err, &recordHeader) || errors.As(err, &certVerify) || strings.Contains(err.Error(), "tls:") {
		return domain.FailTLS
	}
	return domain.FailConnection
}

// parseRetryAfter reads a Retry-After header, which is either a number of seconds or a date. A missing,
// unreadable or past value gives zero.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
