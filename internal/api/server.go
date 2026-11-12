package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sungwoonpk/bowler/internal/dispatcher"
	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/retention"
	"github.com/sungwoonpk/bowler/internal/ssrf"
	"github.com/sungwoonpk/bowler/internal/store"
)

// MinAPIKeyLength is the shortest API key accepted. The key guards everything, so a short one is refused.
const MinAPIKeyLength = 24

// Config is what the API needs to run.
type Config struct {
	Store *store.Store
	// APIKey is the bearer token that protects the API.
	APIKey string
	// SSRF decides which endpoint URLs may be registered. The zero value allows public https URLs only.
	SSRF ssrf.Policy
	// Notify is called after a message is stored, so the dispatcher can start on it without waiting for its next poll.
	Notify func()
	// Stats reports the dispatcher's counters for /metrics. Optional.
	Stats func() dispatcher.Stats
	// Retention reports what the retention janitor has deleted, for /metrics. Optional.
	Retention func() retention.Stats
	// MaxPayloadBytes is the largest message payload accepted. Default 256 KB.
	MaxPayloadBytes int
	Version         string
	Now             func() time.Time
	Log             *slog.Logger
}

// Server is the HTTP interface.
type Server struct {
	store     *store.Store
	apiKey    string
	policy    ssrf.Policy
	notify    func()
	stats     func() dispatcher.Stats
	retention func() retention.Stats
	// auditErrors counts audit entries that could not be recorded.
	auditErrors atomic.Uint64
	// lastTouched remembers, for each token, when its use was last written down.
	lastTouched sync.Map
	// recordAudit writes one audit entry. It is the store's, and a test replaces it to see what a failure does.
	recordAudit func(context.Context, domain.AuditEntry, time.Time) (domain.AuditEntry, error)
	maxPayload  int
	version     string
	now         func() time.Time
	log         *slog.Logger
	failures    *failureTracker
	mux         *http.ServeMux
	registered  []string // "METHOD /path" for every route, so a test can compare them with the OpenAPI file
}

// New builds the server. It refuses a configuration that would leave the API unprotected.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("api: a store is required")
	}
	if len(cfg.APIKey) < MinAPIKeyLength {
		return nil, errors.New("api: the API key must be at least 24 characters, for example from `openssl rand -hex 24`")
	}
	s := &Server{
		store: cfg.Store, apiKey: cfg.APIKey, policy: cfg.SSRF, notify: cfg.Notify, stats: cfg.Stats, retention: cfg.Retention,
		maxPayload: cfg.MaxPayloadBytes, version: cfg.Version, now: cfg.Now, log: cfg.Log,
		failures: newFailureTracker(10, time.Minute), mux: http.NewServeMux(),
	}
	if s.maxPayload <= 0 {
		s.maxPayload = 256 << 10
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.notify == nil {
		s.notify = func() {}
	}
	s.recordAudit = cfg.Store.RecordAudit
	s.routes()
	return s, nil
}

// Handler returns the handler to serve.
func (s *Server) Handler() http.Handler { return s.middleware(http.HandlerFunc(s.route)) }

// route sends a request to its handler. When no route fits, the router's own answer is a plain text 404 or 405, so
// it is asked what it would say and the answer is given again in the JSON shape every other error has.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	h, pattern := s.mux.Handler(r)
	if pattern != "" {
		// ServeHTTP on the mux, not on h: only the mux fills in the path values such as {app}.
		s.mux.ServeHTTP(w, r)
		return
	}
	probe := &statusProbe{header: http.Header{}}
	h.ServeHTTP(probe, r)
	if probe.status == http.StatusMethodNotAllowed {
		e := newError(http.StatusMethodNotAllowed, "method_not_allowed", "this route does not accept "+r.Method)
		e.headers = map[string]string{"Allow": probe.header.Get("Allow")}
		writeError(w, r, e)
		return
	}
	writeError(w, r, errNotFound)
}

// statusProbe records the status a handler would send and throws the body away.
type statusProbe struct {
	header http.Header
	status int
}

func (p *statusProbe) Header() http.Header         { return p.header }
func (p *statusProbe) Write(b []byte) (int, error) { return len(b), nil }
func (p *statusProbe) WriteHeader(code int)        { p.status = code }

// Routes lists every route as "METHOD /path/{param}", the same notation the OpenAPI file uses.
func (s *Server) Routes() []string {
	out := append([]string(nil), s.registered...)
	sort.Strings(out)
	return out
}

func (s *Server) routes() {
	protected := func(pattern string, h handler) {
		s.registered = append(s.registered, pattern)
		s.mux.HandleFunc(pattern, s.wrap(s.auth(h)))
	}
	// adminOnly routes are for the operator's key alone. protected ones also take a token, but only for its own app:
	// every handler that has {app} in its path goes through s.app, which enforces that.
	adminOnly := func(pattern string, h handler) {
		s.registered = append(s.registered, pattern)
		s.mux.HandleFunc(pattern, s.wrap(s.adminOnly(h)))
	}
	public := func(pattern string, h http.HandlerFunc) {
		s.registered = append(s.registered, pattern)
		s.mux.HandleFunc(pattern, h)
	}

	adminOnly("POST /v1/apps", s.createApp)
	adminOnly("GET /v1/apps", s.listApps)
	protected("GET /v1/apps/{app}", s.getApp)

	adminOnly("POST /v1/apps/{app}/tokens", s.createToken)
	adminOnly("GET /v1/apps/{app}/tokens", s.listTokens)
	adminOnly("DELETE /v1/apps/{app}/tokens/{token}", s.revokeToken)

	protected("GET /v1/apps/{app}/audit-log", s.listAppAudit)
	adminOnly("GET /v1/audit-log", s.listAudit)

	protected("POST /v1/apps/{app}/endpoints", s.createEndpoint)
	protected("GET /v1/apps/{app}/endpoints", s.listEndpoints)
	protected("GET /v1/apps/{app}/endpoints/{endpoint}", s.getEndpoint)
	protected("PATCH /v1/apps/{app}/endpoints/{endpoint}", s.updateEndpoint)
	protected("DELETE /v1/apps/{app}/endpoints/{endpoint}", s.deleteEndpoint)
	protected("GET /v1/apps/{app}/endpoints/{endpoint}/secret", s.getSecret)
	protected("POST /v1/apps/{app}/endpoints/{endpoint}/secret/rotate", s.rotateSecret)
	protected("POST /v1/apps/{app}/endpoints/{endpoint}/recover", s.recoverEndpoint)
	protected("GET /v1/apps/{app}/endpoints/{endpoint}/deliveries", s.listEndpointDeliveries)

	protected("POST /v1/apps/{app}/messages", s.createMessage)
	protected("GET /v1/apps/{app}/messages", s.listMessages)
	protected("GET /v1/apps/{app}/messages/{message}", s.getMessage)
	protected("GET /v1/apps/{app}/messages/{message}/deliveries", s.listMessageDeliveries)

	protected("GET /v1/apps/{app}/deliveries/{delivery}", s.getDelivery)
	protected("GET /v1/apps/{app}/deliveries/{delivery}/attempts", s.listAttempts)
	protected("POST /v1/apps/{app}/deliveries/{delivery}/replay", s.replayDelivery)

	adminOnly("GET /metrics", s.metrics)
	public("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	public("GET /readyz", s.wrap(s.ready))
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		return newError(http.StatusServiceUnavailable, "not_ready", "the database is not answering")
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	return nil
}

// list is the shape of every list response.
type list[T any] struct {
	Data       []T    `json:"data"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func newList[T any](items []T, next string) list[T] {
	if items == nil {
		items = []T{}
	}
	return list[T]{Data: items, NextCursor: next}
}
