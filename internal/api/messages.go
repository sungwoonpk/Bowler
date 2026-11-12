package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/id"
	"github.com/sungwoonpk/bowler/internal/store"
)

// Messages: the events that are sent.

// messageResponse is a message with a summary of where it went.
type messageResponse struct {
	domain.Message
	Deliveries []deliverySummary `json:"deliveries"`
}

type deliverySummary struct {
	ID         string       `json:"id"`
	EndpointID string       `json:"endpoint_id"`
	State      domain.State `json:"state"`
}

func summarize(ds []domain.Delivery) []deliverySummary {
	out := make([]deliverySummary, 0, len(ds))
	for _, d := range ds {
		out = append(out, deliverySummary{ID: d.ID, EndpointID: d.EndpointID, State: d.State})
	}
	return out
}

func (s *Server) createMessage(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	var in struct {
		EventType string          `json:"event_type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if !eventTypePattern.MatchString(in.EventType) {
		return badRequest("event_type may use letters, digits and . _ : - and be at most 128 characters")
	}
	if ae := validPayload(in.Payload, s.maxPayload); ae != nil {
		return ae
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > maxIdempotencyKey || strings.ContainsFunc(key, func(c rune) bool { return c < 0x20 || c == 0x7f }) {
		return badRequest("Idempotency-Key must be at most 255 printable characters")
	}

	msg, ds, created, err := s.store.CreateMessage(r.Context(), store.NewMessage{
		AppID: app.ID, EventType: in.EventType, Payload: in.Payload, IdempotencyKey: key,
	}, s.now())
	if err != nil {
		return err
	}
	if created {
		s.notify()
	} else {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, http.StatusAccepted, messageResponse{Message: msg, Deliveries: summarize(ds)})
	return nil
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	p, ae := page(r)
	if ae != nil {
		return ae
	}
	eventType := r.URL.Query().Get("event_type")
	if eventType != "" && !eventTypePattern.MatchString(eventType) {
		return badRequest("event_type is not valid")
	}
	msgs, next, err := s.store.ListMessages(r.Context(), app.ID, eventType, p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, newList(msgs, next))
	return nil
}

func (s *Server) getMessage(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	mID, err := pathID(r, "message", id.Message)
	if err != nil {
		return err
	}
	msg, err := s.store.GetMessage(r.Context(), app.ID, mID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, msg)
	return nil
}

func (s *Server) listMessageDeliveries(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	mID, err := pathID(r, "message", id.Message)
	if err != nil {
		return err
	}
	ds, err := s.store.ListDeliveriesForMessage(r.Context(), app.ID, mID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, newList(ds, ""))
	return nil
}
