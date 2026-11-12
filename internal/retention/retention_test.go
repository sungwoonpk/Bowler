package retention

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/store"
)

var now = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) (*store.Store, domain.App) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "bowler.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	app, err := s.CreateApp(context.Background(), "acme", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	return s, app
}

// oldMessages adds n messages created the given time ago, with no endpoint so they have nothing left to do.
func oldMessages(t *testing.T, s *store.Store, app domain.App, n int, age time.Duration) {
	t.Helper()
	for range n {
		if _, _, _, err := s.CreateMessage(context.Background(), store.NewMessage{AppID: app.ID, EventType: "x", Payload: json.RawMessage(`{}`)}, now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
}

func messages(t *testing.T, s *store.Store, app domain.App) int {
	t.Helper()
	list, _, err := s.ListMessages(context.Background(), app.ID, "", store.Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

func TestNewRefusesSettingsThatCannotWork(t *testing.T) {
	s, _ := newStore(t)
	if _, err := New(Config{Keep: time.Hour}); err == nil {
		t.Error("a store is required")
	}
	for _, keep := range []time.Duration{0, -time.Hour} {
		if _, err := New(Config{Store: s, Keep: keep}); err == nil {
			t.Errorf("Keep %s should be refused", keep)
		}
	}
}

func TestSweepDeletesWhatIsOlderThanKeepAndNothingElse(t *testing.T) {
	s, app := newStore(t)
	oldMessages(t, s, app, 30, 40*24*time.Hour)
	oldMessages(t, s, app, 5, 2*24*time.Hour)
	j, err := New(Config{Store: s, Keep: 30 * 24 * time.Hour, Batch: 8, Pause: time.Millisecond, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	res, err := j.Sweep(context.Background())
	if err != nil || res.Messages != 30 {
		t.Fatalf("30 old messages go in four batches of at most 8: %+v %v", res, err)
	}
	if left := messages(t, s, app); left != 5 {
		t.Fatalf("the recent messages stay, %d left", left)
	}
	st := j.Stats()
	if st.Sweeps != 1 || st.Messages != 30 || st.Errors != 0 {
		t.Fatalf("%+v", st)
	}
	if res, _ := j.Sweep(context.Background()); res != (store.PurgeResult{}) {
		t.Fatalf("a second sweep finds nothing: %+v", res)
	}
}

func TestSweepStopsWhenAskedTo(t *testing.T) {
	s, app := newStore(t)
	oldMessages(t, s, app, 40, 40*24*time.Hour)
	j, _ := New(Config{Store: s, Keep: 30 * 24 * time.Hour, Batch: 5, Pause: time.Hour, Now: func() time.Time { return now }})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	done := make(chan error, 1)
	go func() { _, err := j.Sweep(ctx); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stopped sweep reports why")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a sweep in its pause must stop when asked, not wait out the pause")
	}
	if left := messages(t, s, app); left != 35 {
		t.Fatalf("the batch before the stop stays deleted, %d left", left)
	}
}

func TestRunSweepsAtStartAndAgainLater(t *testing.T) {
	s, app := newStore(t)
	oldMessages(t, s, app, 3, 40*24*time.Hour)
	j, _ := New(Config{Store: s, Keep: 30 * 24 * time.Hour, Every: 20 * time.Millisecond, Now: func() time.Time { return now }})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { j.Run(ctx); close(done) }()
	deadline := time.After(3 * time.Second)
	for messages(t, s, app) != 0 {
		select {
		case <-deadline:
			t.Fatal("old messages should be removed by the first sweep")
		case <-time.After(10 * time.Millisecond):
		}
	}
	oldMessages(t, s, app, 2, 40*24*time.Hour) // added after the first sweep
	for messages(t, s, app) != 0 {
		select {
		case <-deadline:
			t.Fatal("a later sweep should remove what arrived since")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run must return when its context ends")
	}
}

func TestSweepCountsAFailure(t *testing.T) {
	s, _ := newStore(t)
	j, _ := New(Config{Store: s, Keep: time.Hour, Now: func() time.Time { return now }})
	s.Close() // the database is gone
	if _, err := j.Sweep(context.Background()); err == nil {
		t.Fatal("a failed purge must be reported")
	}
	if j.Stats().Errors != 1 {
		t.Fatalf("%+v", j.Stats())
	}
}
