package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/id"
	"github.com/sungwoonpk/bowler/internal/store"
	"github.com/sungwoonpk/bowler/internal/token"
)

// Tokens: what an app's own side uses to call the API for that app and nothing else. Only the operator's key makes,
// lists and revokes them, so a token cannot make another.

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

// maxTokenHours is the longest a token may be made to last: a year.
const maxTokenHours = 24 * 365

// tokenWithSecret is the one response that carries the token itself. It is never shown again.
type tokenWithSecret struct {
	domain.AppToken
	Token string `json:"token"`
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	var in struct {
		Name           string `json:"name"`
		Scope          string `json:"scope"`
		ExpiresInHours *int   `json:"expires_in_hours"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	name, ae := cleanText("name", in.Name, maxNameRune, true)
	if ae != nil {
		return ae
	}
	scope := in.Scope
	switch scope {
	case "":
		scope = domain.ScopeFull
	case domain.ScopeFull, domain.ScopeRead:
	default:
		return badRequest(`scope must be "full" or "read"`)
	}
	var expires *time.Time
	if in.ExpiresInHours != nil {
		if *in.ExpiresInHours < 1 || *in.ExpiresInHours > maxTokenHours {
			return badRequest("expires_in_hours must be from 1 to " + strconv.Itoa(maxTokenHours) + ", or left out for a token that does not expire")
		}
		t := s.now().Add(time.Duration(*in.ExpiresInHours) * time.Hour)
		expires = &t
	}
	tok := token.New()
	detail := map[string]any{"name": name, "scope": scope}
	if expires != nil {
		detail["expires_at"] = expires.UTC().Format(time.RFC3339)
	}
	made, err := s.store.CreateAppToken(r.Context(), store.NewAppToken{
		AppID: app.ID, Name: name, Hash: tok.Hash, Prefix: tok.Short, Scope: scope, ExpiresAt: expires,
	}, s.now(), store.Audited(s.auditEntry(w, r, app.ID, "token.create", "token", "", detail)))
	if errors.Is(err, store.ErrTooManyTokens) {
		return newError(http.StatusConflict, "too_many_tokens",
			"this app already has "+strconv.Itoa(store.MaxActiveTokensPerApp)+" working tokens, revoke one first")
	}
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, tokenWithSecret{AppToken: made, Token: tok.Secret})
	return nil
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	p, ae := page(r)
	if ae != nil {
		return ae
	}
	tokens, next, err := s.store.ListAppTokens(r.Context(), app.ID, p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, newList(tokens, next))
	return nil
}

// revokeToken stops a token at once. Doing it again changes nothing, so a caller that is not sure it worked can repeat it.
func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	tID, err := pathID(r, "token", id.Token)
	if err != nil {
		return err
	}
	// The name is looked up first because it goes in the audit entry. A name never changes, so this cannot go stale.
	before, err := s.store.GetAppToken(r.Context(), app.ID, tID)
	if err != nil {
		return err
	}
	// The store writes the entry only when this call is the one that revokes, so asking twice, or two callers at once,
	// leave one entry.
	revoked, err := s.store.RevokeAppToken(r.Context(), app.ID, tID, s.now(),
		store.Audited(s.auditEntry(w, r, app.ID, "token.revoke", "token", tID, map[string]any{"name": before.Name})))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, revoked)
	return nil
}
