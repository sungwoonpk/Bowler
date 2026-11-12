package api

import (
	"net/http"

	"github.com/sungwoonpk/bowler/internal/store"
)

// The audit log: who did what, and when.

// listAppAudit shows one app's entries. The operator may ask for any app, and a token only for its own, which s.app checks.
func (s *Server) listAppAudit(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	return s.writeAudit(w, r, app.ID)
}

// listAudit shows every app's entries, or one app's when ?app= names it. Only the operator may use it.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) error {
	appID := ""
	if ref := r.URL.Query().Get("app"); ref != "" {
		if len(ref) > 100 {
			return errNotFound
		}
		app, err := s.store.ResolveApp(r.Context(), ref)
		if err != nil {
			return err
		}
		appID = app.ID
	}
	return s.writeAudit(w, r, appID)
}

func (s *Server) writeAudit(w http.ResponseWriter, r *http.Request, appID string) error {
	p, ae := page(r)
	if ae != nil {
		return ae
	}
	action := r.URL.Query().Get("action")
	if len(action) > 64 {
		return badRequest("action is too long")
	}
	entries, next, err := s.store.ListAudit(r.Context(), store.AuditFilter{AppID: appID, Action: action}, p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, newList(entries, next))
	return nil
}
