// Package domain holds the things Bowler is about, with no knowledge of how they are stored or sent.
package domain

import (
	"encoding/json"
	"time"
)

// MaxConcurrency is the most deliveries one endpoint may have in flight at once. The API refuses a larger setting, and the
// HTTP client keeps this many idle connections per host so that an endpoint working at full concurrency reuses its
// connections instead of opening and closing one for nearly every request.
const MaxConcurrency = 20

// MaxRatePerSecond is the highest rate limit an endpoint can be given. A limit is a whole number of attempts per second,
// and 1000 keeps the gap between two attempts at a whole number of milliseconds or more.
const MaxRatePerSecond = 1000

// App is one of Bowler's customers: a company that sends webhooks to its own customers.
type App struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	UID       *string   `json:"uid,omitempty"` // the caller's own identifier for this app, unique when set
	CreatedAt time.Time `json:"created_at"`
}

// Endpoint is a URL, owned by an app's end customer, that receives webhooks.
type Endpoint struct {
	ID          string `json:"id"`
	AppID       string `json:"app_id"`
	URL         string `json:"url"`
	Description string `json:"description"`
	// EventTypes limits which events are sent here. Nil means every event.
	EventTypes []string `json:"event_types"`
	// Concurrency is how many deliveries to this endpoint may be in flight at once. One means they go out one at a
	// time, which is gentle on a receiver and roughly keeps order.
	Concurrency int `json:"concurrency"`
	// RatePerSecond caps how many attempts start per second, on average. Zero means no limit. Up to one second's
	// worth may go out together after a quiet time.
	RatePerSecond  int        `json:"rate_limit_per_second"`
	DisabledAt     *time.Time `json:"disabled_at,omitempty"`
	DisabledReason string     `json:"disabled_reason,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`

	// Secret and PrevSecret sign what is sent. They are never part of the normal JSON for an endpoint.
	Secret              string     `json:"-"`
	PrevSecret          string     `json:"-"`
	PrevSecretExpiresAt *time.Time `json:"-"`
}

// Disabled reports whether the endpoint is switched off.
func (e Endpoint) Disabled() bool { return e.DisabledAt != nil }

// Wants reports whether an event type should be sent to this endpoint.
func (e Endpoint) Wants(eventType string) bool {
	if e.EventTypes == nil {
		return true
	}
	for _, t := range e.EventTypes {
		if t == eventType {
			return true
		}
	}
	return false
}

// SigningSecrets returns the secrets to sign with at the given moment: the current one, and the previous one while
// its grace period lasts. Signing with both lets a receiver switch secrets without losing a message.
func (e Endpoint) SigningSecrets(now time.Time) []string {
	secrets := []string{e.Secret}
	if e.PrevSecret != "" && e.PrevSecretExpiresAt != nil && now.Before(*e.PrevSecretExpiresAt) {
		secrets = append(secrets, e.PrevSecret)
	}
	return secrets
}

// Message is one event: something that happened, to be told to every endpoint that cares.
type Message struct {
	ID             string          `json:"id"`
	AppID          string          `json:"app_id"`
	EventType      string          `json:"event_type"`
	Payload        json.RawMessage `json:"payload"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// State is where a delivery is in its life.
type State string

const (
	// Pending: waiting for its time, either for the first attempt or for a retry.
	Pending State = "pending"
	// InFlight: an attempt is being made right now, or was when a worker last held it.
	InFlight State = "inflight"
	// Succeeded: the receiver answered with a 2xx.
	Succeeded State = "succeeded"
	// Failed: every attempt failed, or the receiver said it was gone for good. A person can replay it.
	Failed State = "failed"
	// Cancelled: the endpoint was deleted before the delivery finished.
	Cancelled State = "cancelled"
)

// Final reports whether no more attempts will be made without someone asking.
func (s State) Final() bool { return s == Succeeded || s == Failed || s == Cancelled }

// Delivery is one message going to one endpoint. A message sent to three endpoints has three deliveries.
type Delivery struct {
	ID            string     `json:"id"`
	MessageID     string     `json:"message_id"`
	EndpointID    string     `json:"endpoint_id"`
	AppID         string     `json:"app_id"`
	State         State      `json:"state"`
	AttemptCount  int        `json:"attempt_count"`
	NextAttemptAt time.Time  `json:"next_attempt_at"`
	LastStatus    int        `json:"last_status,omitempty"`
	LastFailure   string     `json:"last_failure,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
}

// Trigger says why an attempt was made.
type Trigger string

const (
	// Scheduled attempts follow the retry schedule.
	Scheduled Trigger = "scheduled"
	// Manual attempts come from someone asking for a replay.
	Manual Trigger = "manual"
)

// Failure names why an attempt did not succeed, in terms a person can act on.
type Failure string

const (
	NoFailure        Failure = ""
	FailTimeout      Failure = "timeout" // the receiver did not answer in time
	FailDNS          Failure = "dns"     // the host name did not resolve
	FailRefused      Failure = "connection_refused"
	FailTLS          Failure = "tls"             // the certificate or handshake was not acceptable
	FailBlocked      Failure = "blocked_address" // the address is private or reserved
	FailConnection   Failure = "connection"      // some other network error
	FailRedirect     Failure = "redirect"        // 3xx. Bowler does not follow redirects
	FailClientError  Failure = "http_4xx"        // the receiver rejected it
	FailServerError  Failure = "http_5xx"        // the receiver had a problem
	FailGone         Failure = "gone"            // 410, the receiver says this endpoint no longer exists
	FailRateLimited  Failure = "rate_limited"    // 429
	FailBodyTooLarge Failure = "response_too_large"
	// FailEndpointDisabled marks deliveries that were waiting when their endpoint was switched off for good.
	FailEndpointDisabled Failure = "endpoint_disabled"
)

// Attempt is one try at a delivery, kept so a customer can see what happened.
type Attempt struct {
	ID         string        `json:"id"`
	DeliveryID string        `json:"delivery_id"`
	StartedAt  time.Time     `json:"started_at"`
	Duration   time.Duration `json:"duration_ms"`
	StatusCode int           `json:"status_code,omitempty"`
	Failure    Failure       `json:"failure,omitempty"`
	// ResponseSnippet is the start of the receiver's reply, to help work out what went wrong. It is cut short.
	ResponseSnippet string  `json:"response_snippet,omitempty"`
	Trigger         Trigger `json:"trigger"`
}

// MarshalJSON writes the duration as whole milliseconds, which is what the name promises.
func (a Attempt) MarshalJSON() ([]byte, error) {
	type plain Attempt
	return json.Marshal(struct {
		plain
		Duration int64 `json:"duration_ms"`
	}{plain: plain(a), Duration: a.Duration.Milliseconds()})
}
