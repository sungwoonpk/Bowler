package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/id"
	"github.com/sungwoonpk/bowler/internal/store"
	"github.com/sungwoonpk/bowler/signature"
)

// Endpoints: the URLs that receive webhooks, with their secrets and settings.

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
		RateLimit   int       `json:"rate_limit_per_second"`
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
	if in.Concurrency < 0 || in.Concurrency > domain.MaxConcurrency {
		return badRequest("concurrency must be from 1 to " + strconv.Itoa(domain.MaxConcurrency))
	}
	if ae := validRate(in.RateLimit); ae != nil {
		return ae
	}
	secret, err := signature.GenerateSecret()
	if err != nil {
		return err
	}
	ep, err := s.store.CreateEndpoint(r.Context(), store.NewEndpoint{
		AppID: app.ID, URL: in.URL, Description: desc, EventTypes: types, Concurrency: in.Concurrency, RatePerSecond: in.RateLimit, Secret: secret,
	}, s.now())
	if err != nil {
		return err
	}
	s.audit(w, r, app.ID, "endpoint.create", "endpoint", ep.ID, map[string]any{"url": ep.URL})
	writeJSON(w, http.StatusCreated, endpointWithSecret{Endpoint: ep, Secret: secret})
	return nil
}

// validRate checks a rate limit. Zero means no limit.
func validRate(n int) *apiError {
	if n < 0 || n > domain.MaxRatePerSecond {
		return badRequest("rate_limit_per_second must be 0 for no limit, or from 1 to " + strconv.Itoa(domain.MaxRatePerSecond))
	}
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
		RateLimit   *int            `json:"rate_limit_per_second"`
		Disabled    *bool           `json:"disabled"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	var up store.EndpointUpdate
	// The names of what was asked to change go in the audit log, never the values, which can hold a URL with a token in it.
	var changed []string
	for name, set := range map[string]bool{
		"url": in.URL != nil, "description": in.Description != nil, "event_types": in.EventTypes != nil,
		"concurrency": in.Concurrency != nil, "rate_limit_per_second": in.RateLimit != nil, "disabled": in.Disabled != nil,
	} {
		if set {
			changed = append(changed, name)
		}
	}
	sort.Strings(changed)
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
		if *in.Concurrency < 1 || *in.Concurrency > domain.MaxConcurrency {
			return badRequest("concurrency must be from 1 to " + strconv.Itoa(domain.MaxConcurrency))
		}
		up.Concurrency = in.Concurrency
	}
	if in.RateLimit != nil {
		if ae := validRate(*in.RateLimit); ae != nil {
			return ae
		}
		up.RatePerSecond = in.RateLimit
	}
	up.Disabled = in.Disabled
	updated, err := s.store.UpdateEndpoint(r.Context(), app.ID, ep.ID, up, s.now())
	if err != nil {
		return err
	}
	if in.Disabled != nil && !*in.Disabled {
		s.notify() // switching an endpoint back on makes its waiting deliveries ready
	}
	s.audit(w, r, app.ID, "endpoint.update", "endpoint", ep.ID, map[string]any{"fields": changed})
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
	s.audit(w, r, app.ID, "endpoint.delete", "endpoint", ep.ID, nil)
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
	// Reading a secret is worth a record: whoever holds it can forge webhooks to this endpoint.
	s.audit(w, r, app.ID, "endpoint.secret_read", "endpoint", ep.ID, nil)
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
	s.audit(w, r, app.ID, "endpoint.secret_rotate", "endpoint", ep.ID, map[string]any{"grace_hours": grace})
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
	s.audit(w, r, app.ID, "endpoint.recover", "endpoint", ep.ID, map[string]any{"replayed": n})
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
