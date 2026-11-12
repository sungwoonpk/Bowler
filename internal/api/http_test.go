package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIPIgnoresProxyHeaders(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.9:51234"
	r.Header.Set("X-Forwarded-For", "10.0.0.1")
	r.Header.Set("X-Real-IP", "10.0.0.2")
	if got := clientIP(r); got != "203.0.113.9" {
		t.Fatalf("anyone can send these headers, so only the connection counts, got %q", got)
	}
	r.RemoteAddr = "no-port"
	if got := clientIP(r); got != "no-port" {
		t.Fatalf("got %q", got)
	}
}

func TestRetryAfterSecondsIsAtLeastOne(t *testing.T) {
	for in, want := range map[time.Duration]string{0: "1", 200 * time.Millisecond: "1", time.Second: "1", 90 * time.Second: "90", 59*time.Second + 600*time.Millisecond: "60"} {
		if got := retryAfterSeconds(in); got != want {
			t.Errorf("%v -> %q, want %q", in, got, want)
		}
	}
}

func TestFailureTrackerWindowAndBound(t *testing.T) {
	f := newFailureTracker(3, time.Minute)
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	for range 3 {
		f.record("1.2.3.4", now)
	}
	if f.blocked("1.2.3.4", now) <= 0 {
		t.Fatal("three failures reach the limit")
	}
	if f.blocked("5.6.7.8", now) != 0 {
		t.Fatal("another address is not affected")
	}
	if wait := f.blocked("1.2.3.4", now.Add(30*time.Second)); wait <= 0 || wait > 30*time.Second {
		t.Fatalf("it should say how long is left, got %v", wait)
	}
	if f.blocked("1.2.3.4", now.Add(61*time.Second)) != 0 {
		t.Fatal("failures older than the window are forgotten")
	}
	// A flood of different sources must not grow the map without limit.
	for i := range 12_000 {
		f.record(string(rune(i))+"x", now)
	}
	if len(f.entries) > 10_001 {
		t.Fatalf("the tracker kept %d addresses", len(f.entries))
	}
}

func TestErrorTypeAndRecorder(t *testing.T) {
	e := newError(418, "teapot", "short and stout")
	if e.Error() != "short and stout" {
		t.Fatalf("%q", e.Error())
	}
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: 200}
	sr.WriteHeader(http.StatusTeapot)
	sr.Flush()
	if sr.status != http.StatusTeapot || !rec.Flushed {
		t.Fatalf("status %d flushed %v", sr.status, rec.Flushed)
	}
}
