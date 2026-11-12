package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sungwoonpk/bowler/internal/id"
	"github.com/sungwoonpk/bowler/internal/store"
)

var (
	uidPattern       = regexp.MustCompile(`^[A-Za-z0-9_.:@-]{1,100}$`)
	eventTypePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
)

const (
	maxEventTypes      = 50
	maxConcurrency     = 20
	maxIdempotencyKey  = 255
	maxDescriptionRune = 500
	maxNameRune        = 100
)

// cleanText trims a name or description and refuses control characters, which have no business in either and
// can confuse logs and terminals.
func cleanText(field, v string, max int, required bool) (string, *apiError) {
	v = strings.TrimSpace(v)
	if required && v == "" {
		return "", badRequest(field + " is required")
	}
	if utf8.RuneCountInString(v) > max {
		return "", badRequest(field + " must be at most " + strconv.Itoa(max) + " characters")
	}
	if !utf8.ValidString(v) || strings.IndexFunc(v, unicode.IsControl) >= 0 {
		return "", badRequest(field + " contains characters that are not allowed")
	}
	return v, nil
}

func validEventTypes(types []string) *apiError {
	if len(types) > maxEventTypes {
		return badRequest("event_types may list at most " + strconv.Itoa(maxEventTypes) + " types")
	}
	for _, t := range types {
		if !eventTypePattern.MatchString(t) {
			return badRequest("event type " + strconv.Quote(t) + " may use letters, digits and . _ : - and be at most 128 characters")
		}
	}
	return nil
}

// page reads ?limit= and ?cursor=.
func page(r *http.Request) (store.Page, *apiError) {
	p := store.Page{Cursor: r.URL.Query().Get("cursor")}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 250 {
			return p, badRequest("limit must be a number from 1 to 250")
		}
		p.Limit = n
	}
	return p, nil
}

// pathID returns a path value, and refuses one that cannot be an id of the right kind. A malformed id is a 404
// at once, without asking the database.
func pathID(r *http.Request, name, prefix string) (string, error) {
	v := r.PathValue(name)
	if !id.HasPrefix(v, prefix) {
		return "", errNotFound
	}
	return v, nil
}

// validPayload checks a message payload: valid JSON, not null, and within the size limit.
func validPayload(raw json.RawMessage, max int) *apiError {
	if len(raw) == 0 {
		return badRequest("payload is required")
	}
	if len(raw) > max {
		return newError(http.StatusRequestEntityTooLarge, "payload_too_large", "payload must be at most "+strconv.Itoa(max)+" bytes")
	}
	if string(raw) == "null" {
		return badRequest("payload must not be null")
	}
	if !json.Valid(raw) {
		return badRequest("payload is not valid JSON")
	}
	return nil
}
