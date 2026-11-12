package dispatcher

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sungwoonpk/bowler/internal/deliver"
	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/ssrf"
	"github.com/sungwoonpk/bowler/internal/store"
)

// The benchmarks send b.N messages, each to `fanOut` endpoints, and time how long it takes for every delivery to be
// recorded as succeeded. They measure the whole path: storing the message, claiming, sending, recording the result.
//
//	go test ./internal/dispatcher -run '^$' -bench . -benchtime 3000x -count 3
const fanOut = 10

func benchSetup(b *testing.B, urls func(i int) string) (*store.Store, domain.App) {
	b.Helper()
	s, err := store.Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	app, err := s.CreateApp(context.Background(), "bench", nil, time.Now())
	if err != nil {
		b.Fatal(err)
	}
	for i := range fanOut {
		if _, err := s.CreateEndpoint(context.Background(), store.NewEndpoint{AppID: app.ID, URL: urls(i), Concurrency: 20, Secret: "whsec_c2VjcmV0"}, time.Now()); err != nil {
			b.Fatal(err)
		}
	}
	return s, app
}

func runEndToEnd(b *testing.B, sender Sender, urls func(int) string) {
	s, app := benchSetup(b, urls)
	d, err := New(s, sender, Config{Workers: 128, PollInterval: 5 * time.Millisecond, AttemptTimeout: 5 * time.Second, LeaseFor: 30 * time.Second})
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	payload := json.RawMessage(`{"invoice":"in_1","amount":4200,"currency":"usd"}`)
	want := b.N * fanOut
	b.ResetTimer()
	start := time.Now()
	for range b.N {
		if _, _, _, err := s.CreateMessage(ctx, store.NewMessage{AppID: app.ID, EventType: "invoice.paid", Payload: payload}, time.Now()); err != nil {
			b.Fatal(err)
		}
		d.Notify()
	}
	enqueued := time.Since(start)
	for {
		counts, err := s.CountByState(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if counts[domain.Succeeded] >= want {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	elapsed := time.Since(start)
	b.StopTimer()
	b.ReportMetric(float64(want)/elapsed.Seconds(), "deliveries/s")
	b.ReportMetric(float64(b.N)/enqueued.Seconds(), "messages/s-accepted")
}

// BenchmarkQueue uses a sender that answers at once, so it measures the queue and the database alone.
func BenchmarkQueue(b *testing.B) {
	instant := fakeSender(func(_ context.Context, c store.Claim) deliver.Outcome { return ok(c) })
	runEndToEnd(b, instant, func(int) string { return "https://example.com/hook" })
}

// BenchmarkHTTP sends real signed requests over the loopback interface to a receiver that answers 200.
func BenchmarkHTTP(b *testing.B) {
	var connections atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	sender := deliver.New(deliver.Config{SSRF: ssrf.Policy{AllowPrivate: true}, Timeout: 5 * time.Second})
	defer sender.Close()
	runEndToEnd(b, sender, func(int) string { return srv.URL })
	// How many new connections the receiver had to accept for each thousand deliveries. Reusing connections keeps this
	// low. A high number means connections are opened and closed for nearly every request.
	b.ReportMetric(float64(connections.Load())/float64(b.N*fanOut)*1000, "conns/1000-deliveries")
}
