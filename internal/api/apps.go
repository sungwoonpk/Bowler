package api

import (
	"net/http"
	"strings"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/id"
	"github.com/sungwoonpk/bowler/internal/store"
)

// Apps: the customers of the company that runs Bowler.

func (s *Server) createApp(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name string  `json:"name"`
		UID  *string `json:"uid"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	name, ae := cleanText("name", in.Name, maxNameRune, true)
	if ae != nil {
		return ae
	}
	if in.UID != nil {
		if !uidPattern.MatchString(*in.UID) || strings.HasPrefix(*in.UID, id.App+"_") {
			// An uid that looks like an app id could be confused with another app's real id.
			return badRequest("uid may use letters, digits and . _ : @ -, be at most 100 characters and not start with app_")
		}
	}
	app, err := s.store.CreateApp(r.Context(), name, in.UID, s.now(), store.Audited(s.auditEntry(w, r, "", "app.create", "app", "", nil)))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, app)
	return nil
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) error {
	p, ae := page(r)
	if ae != nil {
		return ae
	}
	apps, next, err := s.store.ListApps(r.Context(), p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, newList(apps, next))
	return nil
}

// app finds the app named in the path, by id or by the caller's own uid.
func (s *Server) app(r *http.Request) (domain.App, error) {
	ref := r.PathValue("app")
	if ref == "" || len(ref) > 100 {
		return domain.App{}, errNotFound
	}
	app, err := s.store.ResolveApp(r.Context(), ref)
	if err != nil {
		return domain.App{}, err
	}
	// A token is for one app. Another app looks exactly like one that does not exist, so a token cannot be used to find
	// out which apps there are.
	if p := callerOf(r); !p.admin && p.appID != app.ID {
		return domain.App{}, errNotFound
	}
	return app, nil
}

func (s *Server) getApp(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, app)
	return nil
}
