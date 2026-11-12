package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/id"
	"github.com/sungwoonpk/bowler/internal/store"
	"github.com/sungwoonpk/bowler/signature"
)

// ---- apps ----

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
	app, err := s.store.CreateApp(r.Context(), name, in.UID, s.now())
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
	return s.store.ResolveApp(r.Context(), ref)
}

func (s *Server) getApp(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, app)
	return nil
}

// ---- endpoints ----

// endpointWithSecret is an endpoint plus its signing secret. The secret appears in the response that creates the
// endpoint and in the one that reads it back on purpose, and nowhere else.
type endpointWithSecret struct {
	domain.Endpoint
	Secret string `json:"secret"`
}

func (s *Server) endpoint(r *http.Request, app domain.App) (domain.Endpoint, error) {
	epID, err := pathID(r, "endpoint", id.Endpoint)
	if err != nil {
		return domain.Endpoint{}, err
	}
	return s.store.GetEndpoint(r.Context(), app.ID, epID)
}

func (s *Server) createEndpoint(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	var in struct {
		URL         string    `json:"url"`
		Description string    `json:"description"`
		EventTypes  *[]string `json:"event_types"`
		Concurrency int       `json:"concurrency"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if _, err := s.policy.ValidateURL(in.URL); err != nil {
		return badRequest("url: " + err.Error())
	}
	desc, ae := cleanText("description", in.Description, maxDescriptionRune, false)
	if ae != nil {
		return ae
	}
	var types []string
	if in.EventTypes != nil {
		types = *in.EventTypes
		if ae := validEventTypes(types); ae != nil {
			return ae
		}
		if types == nil {
			types = []string{}
		}
	}
	if in.Concurrency < 0 || in.Concurrency > maxConcurrency {
		return badRequest("concurrency must be from 1 to " + strconv.Itoa(maxConcurrency))
	}
	secret, err := signature.GenerateSecret()
	if err != nil {
		return err
	}
	ep, err := s.store.CreateEndpoint(r.Context(), store.NewEndpoint{
		AppID: app.ID, URL: in.URL, Description: desc, EventTypes: types, Concurrency: in.Concurrency, Secret: secret,
	}, s.now())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, endpointWithSecret{Endpoint: ep, Secret: secret})
	return nil
}

func (s *Server) listEndpoints(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	p, ae := page(r)
	if ae != nil {
		return ae
	}
	eps, next, err := s.store.ListEndpoints(r.Context(), app.ID, p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, newList(eps, next))
	return nil
}

func (s *Server) getEndpoint(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	ep, err := s.endpoint(r, app)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, ep)
	return nil
}

func (s *Server) updateEndpoint(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	ep, err := s.endpoint(r, app)
	if err != nil {
		return err
	}
	// event_types is raw so that "leave it alone" (absent) and "every event" (null) can be told apart.
	var in struct {
		URL         *string         `json:"url"`
		Description *string         `json:"description"`
		EventTypes  json.RawMessage `json:"event_types"`
		Concurrency *int            `json:"concurrency"`
		Disabled    *bool           `json:"disabled"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	var up store.EndpointUpdate
	if in.URL != nil {
		if _, err := s.policy.ValidateURL(*in.URL); err != nil {
			return badRequest("url: " + err.Error())
		}
		up.URL = in.URL
	}
	if in.Description != nil {
		d, ae := cleanText("description", *in.Description, maxDescriptionRune, false)
		if ae != nil {
			return ae
		}
		up.Description = &d
	}
	if in.EventTypes != nil {
		var types *[]string
		if err := json.Unmarshal(in.EventTypes, &types); err != nil {
			return badRequest("event_types must be a list of strings, or null for every event")
		}
		if types != nil {
			if ae := validEventTypes(*types); ae != nil {
				return ae
			}
			if *types == nil {
				empty := []string{}
				types = &empty
			}
		}
		up.EventTypes = &types
	}
	if in.Concurrency != nil {
		if *in.Concurrency < 1 || *in.Concurrency > maxConcurrency {
			return badRequest("concurrency must be from 1 to " + strconv.Itoa(maxConcurrency))
		}
		up.Concurrency = in.Concurrency
	}
	up.Disabled = in.Disabled
	updated, err := s.store.UpdateEndpoint(r.Context(), app.ID, ep.ID, up, s.now())
	if err != nil {
		return err
	}
	if in.Disabled != nil && !*in.Disabled {
		s.notify() // switching an endpoint back on makes its waiting deliveries ready
	}
	writeJSON(w, http.StatusOK, updated)
	return nil
}

func (s *Server) deleteEndpoint(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	ep, err := s.endpoint(r, app)
	if err != nil {
		return err
	}
	if err := s.store.DeleteEndpoint(r.Context(), app.ID, ep.ID, s.now()); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) getSecret(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	ep, err := s.endpoint(r, app)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]string{"secret": ep.Secret})
	return nil
}

const (
	defaultGraceHours = 24
	maxGraceHours     = 24 * 7
)

func (s *Server) rotateSecret(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	ep, err := s.endpoint(r, app)
	if err != nil {
		return err
	}
	var in struct {
		GraceHours *int `json:"grace_hours"`
	}
	if err := decodeOptional(w, r, &in); err != nil {
		return err
	}
	grace := defaultGraceHours
	if in.GraceHours != nil {
		grace = *in.GraceHours
	}
	if grace < 0 || grace > maxGraceHours {
		return badRequest("grace_hours must be from 0 to " + strconv.Itoa(maxGraceHours))
	}
	secret, err := signature.GenerateSecret()
	if err != nil {
		return err
	}
	until := s.now().Add(time.Duration(grace) * time.Hour)
	updated, err := s.store.RotateSecret(r.Context(), app.ID, ep.ID, secret, until)
	if err != nil {
		return err
	}
	out := map[string]any{"secret": secret}
	if grace > 0 {
		out["previous_secret_valid_until"] = updated.PrevSecretExpiresAt
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) recoverEndpoint(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	ep, err := s.endpoint(r, app)
	if err != nil {
		return err
	}
	var in struct {
		Since *time.Time `json:"since"`
	}
	if err := decodeOptional(w, r, &in); err != nil {
		return err
	}
	now := s.now()
	since := now.Add(-24 * time.Hour)
	if in.Since != nil {
		since = *in.Since
	}
	n, err := s.store.RecoverEndpoint(r.Context(), app.ID, ep.ID, since, now)
	if err != nil {
		return err
	}
	if n > 0 {
		s.notify()
	}
	writeJSON(w, http.StatusOK, map[string]any{"replayed": n, "since": since.UTC()})
	return nil
}

func (s *Server) listEndpointDeliveries(w http.ResponseWriter, r *http.Request) error {
	app, err := s.app(r)
	if err != nil {
		return err
	}
	epID, err := pathID(r, "endpoint", id.Endpoint)
	if err != nil {
		return err
	}
	p, ae := page(r)
	if ae != nil {
		return ae
	}
	state := domain.State(r.URL.Query().Get("state"))
	switch state {
	case "", domain.Pending, domain.InFlight, domain.Succeeded, domain.Failed, domain.Cancelled:
	default:
		return badRequest("state must be one of pending, inflight, succeeded, failed, cancelled")
	}
	ds, next, err := s.store.ListDeliveriesForEndpoint(r.Context(), app.ID, epID, state, p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, newList(ds, next))
	return nil
}

// ---- messages ----

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

// ---- deliveries ----

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
	d, err := s.store.Replay(r.Context(), app.ID, dID, s.now())
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

// ---- metrics ----

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) error {
	counts, err := s.store.CountByState(r.Context())
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# HELP bowler_deliveries Deliveries by state.\n# TYPE bowler_deliveries gauge\n")
	for _, st := range []domain.State{domain.Pending, domain.InFlight, domain.Succeeded, domain.Failed, domain.Cancelled} {
		fmt.Fprintf(&b, "bowler_deliveries{state=%q} %d\n", string(st), counts[st])
	}
	if s.stats != nil {
		st := s.stats()
		b.WriteString("# HELP bowler_attempts_in_flight Attempts being made right now.\n# TYPE bowler_attempts_in_flight gauge\n")
		fmt.Fprintf(&b, "bowler_attempts_in_flight %d\n", st.InFlight)
		b.WriteString("# HELP bowler_attempts_total Attempts finished, by result.\n# TYPE bowler_attempts_total counter\n")
		fmt.Fprintf(&b, "bowler_attempts_total{result=\"succeeded\"} %d\nbowler_attempts_total{result=\"retried\"} %d\n", st.Succeeded, st.Retried)
		fmt.Fprintf(&b, "bowler_attempts_total{result=\"failed\"} %d\nbowler_attempts_total{result=\"aborted\"} %d\n", st.Failed, st.Aborted)
		b.WriteString("# HELP bowler_claimed_total Deliveries taken from the queue.\n# TYPE bowler_claimed_total counter\n")
		fmt.Fprintf(&b, "bowler_claimed_total %d\n", st.Claimed)
		b.WriteString("# HELP bowler_lease_lost_total Results that arrived after the lease had been given to another worker.\n# TYPE bowler_lease_lost_total counter\n")
		fmt.Fprintf(&b, "bowler_lease_lost_total %d\n", st.LeaseLost)
		b.WriteString("# HELP bowler_store_errors_total Database errors while running the queue.\n# TYPE bowler_store_errors_total counter\n")
		fmt.Fprintf(&b, "bowler_store_errors_total %d\n", st.StoreError)
		b.WriteString("# HELP bowler_sender_panics_total Panics recovered while sending.\n# TYPE bowler_sender_panics_total counter\n")
		fmt.Fprintf(&b, "bowler_sender_panics_total %d\n", st.Panics)
	}
	b.WriteString("# HELP bowler_build_info The running version.\n# TYPE bowler_build_info gauge\n")
	fmt.Fprintf(&b, "bowler_build_info{version=%q} 1\n", s.version)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
	return nil
}
