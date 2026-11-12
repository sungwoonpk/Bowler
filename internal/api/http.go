// Package api is Bowler's HTTP interface: the routes a company uses to set up apps and endpoints, send messages,
// and look at what happened to them.
package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sungwoonpk/bowler/internal/store"
)

// apiError is an error that maps straight to an HTTP response.
type apiError struct {
	status  int
	code    string
	message string
	headers map[string]string
}

func (e *apiError) Error() string { return e.message }

func newError(status int, code, message string) *apiError {
	return &apiError{status: status, code: code, message: message}
}

var (
	errUnauthorized = newError(http.StatusUnauthorized, "unauthorized", "a valid API key is required")
	errNotFound     = newError(http.StatusNotFound, "not_found", "no such resource")
	errForbidden    = newError(http.StatusForbidden, "forbidden", "this credential may not do this")
	errReadOnly     = newError(http.StatusForbidden, "read_only", "this token is read only")
)

func badRequest(message string) *apiError {
	return newError(http.StatusBadRequest, "invalid_request", message)
}

// errorBody is the JSON shape of every error.
type errorBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":{"code":"internal","message":"could not encode the response"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

func writeError(w http.ResponseWriter, r *http.Request, e *apiError) {
	var body errorBody
	body.Error.Code, body.Error.Message, body.Error.RequestID = e.code, e.message, w.Header().Get("X-Request-ID")
	for k, v := range e.headers {
		w.Header().Set(k, v)
	}
	writeJSON(w, e.status, body)
}

// handler is a route handler that returns its error instead of writing it, so no path can forget to answer.
type handler func(w http.ResponseWriter, r *http.Request) error

func (s *Server) wrap(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := h(w, r)
		if err == nil {
			return
		}
		var ae *apiError
		switch {
		case errors.As(err, &ae):
		case errors.Is(err, store.ErrNotFound):
			ae = errNotFound
		case errors.Is(err, store.ErrConflict):
			ae = newError(http.StatusConflict, "conflict", "a resource with this identifier already exists")
		case errors.Is(err, store.ErrBadCursor):
			ae = badRequest("the cursor is not valid")
		case errors.Is(err, store.ErrTooManyTokens):
			ae = newError(http.StatusConflict, "too_many_tokens", "this app already has the most tokens it may hold")
		case errors.Is(err, store.ErrBadState):
			ae = newError(http.StatusConflict, "bad_state", "this is not allowed in the delivery's current state")
		default:
			// The details can contain table names and file paths, so they go to the log and not to the caller.
			s.log.Error("request failed", "path", r.URL.Path, "error", err, "request_id", w.Header().Get("X-Request-ID"))
			ae = newError(http.StatusInternalServerError, "internal", "something went wrong on our side")
		}
		writeError(w, r, ae)
	}
}

const maxBodyBytes = 1 << 20

// decode reads a JSON request body into v. It is strict on purpose: an unknown field is almost always a typo, and
// silently ignoring it would make the caller think a setting was applied.
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return newError(http.StatusUnsupportedMediaType, "unsupported_media_type", "send the body as application/json")
	}
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		var syn *json.SyntaxError
		var typ *json.UnmarshalTypeError
		switch {
		case errors.As(err, &tooBig):
			return newError(http.StatusRequestEntityTooLarge, "too_large", "the request body is too large")
		case errors.Is(err, io.EOF):
			return badRequest("the request body is empty")
		case errors.As(err, &syn):
			return badRequest("the request body is not valid JSON")
		case errors.As(err, &typ):
			return badRequest("the field " + typ.Field + " has the wrong type")
		default:
			return badRequest(err.Error())
		}
	}
	if dec.More() {
		return badRequest("the request body must contain one JSON value")
	}
	return nil
}

// emptyBody accepts a request that may have no body, for routes whose body is optional.
func decodeOptional(w http.ResponseWriter, r *http.Request, v any) error {
	if r.ContentLength == 0 {
		return nil
	}
	if err := decode(w, r, v); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.message == "the request body is empty" {
			return nil
		}
		return err
	}
	return nil
}

func requestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}

var safeRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// statusRecorder remembers the status code so the log can report it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// middleware adds what every response needs: a request id, safe headers, a log line and a safety net for panics.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !safeRequestID.MatchString(id) {
			id = requestID()
		}
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := s.now()
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("handler panicked", "panic", p, "path", r.URL.Path, "request_id", id)
				writeError(rec, r, newError(http.StatusInternalServerError, "internal", "something went wrong on our side"))
			}
			// The query string is left out: cursors and filters can carry identifiers worth not logging.
			s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
				"duration_ms", s.now().Sub(start).Milliseconds(), "request_id", id)
		}()
		next.ServeHTTP(rec, r)
	})
}

// auth requires the API key or an app token. A caller who keeps sending a wrong one is slowed down.
//
// The credential is checked first and the throttle only applies to a wrong one. Checking the throttle first would lock
// out a legitimate caller whenever someone else behind the same address, such as an office network, was guessing.
func (s *Server) auth(next handler) handler { return s.authAs(false, next) }

// adminOnly is auth for routes that only the operator may use. A token is a valid credential and not a permitted one,
// so it is refused with 403 and is not counted as a wrong key.
func (s *Server) adminOnly(next handler) handler { return s.authAs(true, next) }

func (s *Server) authAs(adminOnly bool, next handler) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, ok, err := s.authenticate(r)
		if err != nil {
			return err
		}
		if ok {
			if adminOnly && !p.admin {
				return errForbidden
			}
			// A read-only token may look and may not change anything. What it may read is narrowed again where a read
			// would give away more than it shows, as the signing secret does.
			if p.readOnly && r.Method != http.MethodGet && r.Method != http.MethodHead {
				return errReadOnly
			}
			return next(w, withPrincipal(r, p))
		}
		ip := clientIP(r)
		if wait := s.failures.blocked(ip, s.now()); wait > 0 {
			e := newError(http.StatusTooManyRequests, "too_many_attempts", "too many failed attempts, try again later")
			e.headers = map[string]string{"Retry-After": retryAfterSeconds(wait)}
			return e
		}
		s.failures.record(ip, s.now())
		return errUnauthorized
	}
}

// clientIP is the address that connected. Proxy headers are not trusted here: anyone can send them, and a sender
// who writes a different one each time would never be slowed down.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func retryAfterSeconds(d time.Duration) string {
	secs := int(d.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return strconv.Itoa(secs)
}

// failureTracker counts wrong API keys per address over a short window.
type failureTracker struct {
	mu      sync.Mutex
	window  time.Duration
	limit   int
	entries map[string][]time.Time
}

func newFailureTracker(limit int, window time.Duration) *failureTracker {
	return &failureTracker{window: window, limit: limit, entries: map[string][]time.Time{}}
}

func (f *failureTracker) prune(ip string, now time.Time) []time.Time {
	kept := f.entries[ip][:0]
	for _, t := range f.entries[ip] {
		if now.Sub(t) < f.window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(f.entries, ip)
		return nil
	}
	f.entries[ip] = kept
	return kept
}

// blocked returns how long the address must wait, or zero when it may try.
func (f *failureTracker) blocked(ip string, now time.Time) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	times := f.prune(ip, now)
	if len(times) < f.limit {
		return 0
	}
	return f.window - now.Sub(times[0])
}

func (f *failureTracker) record(ip string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prune(ip, now)
	// A bound on how many addresses are remembered, so a flood of spoofed sources cannot grow the map without limit.
	if len(f.entries) > 10_000 {
		f.entries = map[string][]time.Time{}
	}
	f.entries[ip] = append(f.entries[ip], now)
}
