package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/id"
	"github.com/sungwoonpk/bowler/internal/store"
	"github.com/sungwoonpk/bowler/internal/token"
)

// Tokens: what an app's own side uses to call the API for that app and nothing else. Only the operator's key makes,
// lists and revokes them, so a token cannot make another.

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

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
		Name string `json:"name"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	name, ae := cleanText("name", in.Name, maxNameRune, true)
	if ae != nil {
		return ae
	}
	tok := token.New()
	made, err := s.store.CreateAppToken(r.Context(), app.ID, name, tok.Hash, tok.Short, s.now())
	if errors.Is(err, store.ErrTooManyTokens) {
		return newError(http.StatusConflict, "too_many_tokens",
			"this app already has "+strconv.Itoa(store.MaxActiveTokensPerApp)+" working tokens, revoke one first")
	}
	if err != nil {
		return err
	}
	s.audit(w, r, app.ID, "token.create", "token", made.ID, map[string]any{"name": made.Name})
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
	before, err := s.store.GetAppToken(r.Context(), app.ID, tID)
	if err != nil {
		return err
	}
	revoked, err := s.store.RevokeAppToken(r.Context(), app.ID, tID, s.now())
	if err != nil {
		return err
	}
	if !before.Revoked() {
		s.audit(w, r, app.ID, "token.revoke", "token", revoked.ID, map[string]any{"name": revoked.Name})
	}
	writeJSON(w, http.StatusOK, revoked)
	return nil
}
