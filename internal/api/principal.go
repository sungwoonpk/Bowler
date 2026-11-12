package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/token"
)

// principal is who is calling: the operator, with the API key, or one app, with a token made for it.
type principal struct {
	admin   bool
	appID   string // the one app a token is for. Empty for the operator.
	tokenID string
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
		t, err := s.store.AppTokenByHash(r.Context(), token.Hash(bearer))
		switch {
		case err == nil:
			return principal{appID: t.AppID, tokenID: t.ID}, true, nil
		case !isNotFound(err):
			return principal{}, false, err
		}
	}
	return principal{}, false, nil
}

// audit records something a caller did. It is called after the action succeeded and never fails the request: the action
// has happened, and an error here is counted and logged so that a gap in the record is noticed. detail must never hold a
// secret, a token or a payload.
func (s *Server) audit(w http.ResponseWriter, r *http.Request, appID, action, resourceKind, resourceID string, detail map[string]any) {
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
	if _, err := s.recordAudit(r.Context(), e, s.now()); err != nil {
		s.auditErrors.Add(1)
		s.log.Error("recording an audit entry failed", "action", action, "error", err, "request_id", e.RequestID)
	}
}
