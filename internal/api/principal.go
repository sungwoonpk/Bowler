package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/token"
)

// principal is who is calling: the operator, with the API key, or one app, with a token made for it.
type principal struct {
	admin   bool
	appID   string // the one app a token is for. Empty for the operator.
	tokenID string
	// readOnly is true for a token that may look and not change anything.
	readOnly bool
}

type principalKey struct{}

func withPrincipal(r *http.Request, p principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
}

// callerOf returns who is calling. A request that got past auth always has one. A request that did not has no rights.
func callerOf(r *http.Request) principal {
	p, _ := r.Context().Value(principalKey{}).(principal)
	return p
}

func (p principal) actor() (kind, id string) {
	if p.admin {
		return domain.ActorAdmin, "admin"
	}
	return domain.ActorAppToken, p.tokenID
}

// authenticate says who a request comes from, by what is in its Authorization header. ok is false for a request that
// has no valid credentials, and err is for a database that could not be asked.
func (s *Server) authenticate(r *http.Request) (p principal, ok bool, err error) {
	bearer, has := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !has {
		return principal{}, false, nil
	}
	// Comparing in constant time means the response time does not reveal how much of a guess was right.
	if subtle.ConstantTimeCompare([]byte(bearer), []byte(s.apiKey)) == 1 {
		return principal{admin: true}, true, nil
	}
	if token.Looks(bearer) {
		t, err := s.store.AppTokenByHash(r.Context(), token.Hash(bearer), s.now())
		switch {
		case err == nil:
			s.noteUse(r, t.ID)
			return principal{appID: t.AppID, tokenID: t.ID, readOnly: t.Scope == domain.ScopeRead}, true, nil
		case !isNotFound(err):
			return principal{}, false, err
		}
	}
	return principal{}, false, nil
}

// auditEntry describes something a caller is doing, for the store to write in the same transaction as the change, so
// that either both happen or neither does. A resource id left empty is filled in by the store when the change creates
// it. detail must never hold a secret, a token or a payload.
func (s *Server) auditEntry(w http.ResponseWriter, r *http.Request, appID, action, resourceKind, resourceID string, detail map[string]any) domain.AuditEntry {
	kind, actor := callerOf(r).actor()
	e := domain.AuditEntry{
		ActorKind: kind, ActorID: actor, AppID: appID, Action: action,
		ResourceKind: resourceKind, ResourceID: resourceID, RequestID: w.Header().Get("X-Request-ID"),
	}
	if len(detail) > 0 {
		if b, err := json.Marshal(detail); err == nil {
			e.Detail = b
		}
	}
	return e
}

// auditBefore records something that changes nothing, before it is done: reading a secret. If the entry cannot be
// written the caller is refused, since a read nobody can see is the case an audit log exists to prevent.
func (s *Server) auditBefore(w http.ResponseWriter, r *http.Request, e domain.AuditEntry) error {
	_, err := s.store.RecordAudit(r.Context(), e, s.now())
	return err
}

// lastUsedEvery is how often the use of a token is written down. Writing on every request would turn each one into a
// database write, so the time is approximate, to within this.
const lastUsedEvery = 10 * time.Minute

// noteUse records that a token was used, at most once in lastUsedEvery for each token. A failure is logged and ignored:
// it is bookkeeping and must not stop a request that has a valid token.
func (s *Server) noteUse(r *http.Request, tokenID string) {
	now := s.now()
	if prev, ok := s.lastTouched.Load(tokenID); ok && now.Sub(prev.(time.Time)) < lastUsedEvery {
		return
	}
	s.lastTouched.Store(tokenID, now)
	if err := s.store.TouchAppToken(r.Context(), tokenID, now); err != nil {
		s.log.Warn("recording the use of a token failed", "token", tokenID, "error", err)
	}
}
