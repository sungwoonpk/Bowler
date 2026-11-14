package api

import (
	"errors"
	"net/http"

	"github.com/sungwoonpk/bowler/internal/id"
	"github.com/sungwoonpk/bowler/internal/store"
)

// Deliveries and their attempts: what happened to each message at each endpoint.

func (s *Server) getDelivery(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	dID, err := pathID(r, "delivery", id.Delivery)
	if err != nil {
		return err
	}
	d, err := s.store.GetDelivery(r.Context(), app.ID, dID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, d)
	return nil
}

func (s *Server) listAttempts(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	dID, err := pathID(r, "delivery", id.Delivery)
	if err != nil {
		return err
	}
	as, err := s.store.ListAttempts(r.Context(), app.ID, dID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, newList(as, ""))
	return nil
}

func (s *Server) replayDelivery(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	dID, err := pathID(r, "delivery", id.Delivery)
	if err != nil {
		return err
	}
	d, err := s.store.Replay(r.Context(), app.ID, dID, s.now(), store.Audited(s.auditEntry(w, r, app.ID, "delivery.replay", "delivery", dID, nil)))
	if errors.Is(err, store.ErrBadState) {
		return newError(http.StatusConflict, "bad_state", "only a finished delivery can be replayed, this one is still "+string(d.State))
	}
	if err != nil {
		return err
	}
	s.notify()
	writeJSON(w, http.StatusAccepted, d)
	return nil
}
