package retry

import (
	"testing"
	"time"
)

func fixed(v float64) func() float64 { return func() float64 { return v } }

func TestDefaultScheduleShape(t *testing.T) {
	p := Default()
	if p.MaxAttempts() != 8 {
		t.Fatalf("MaxAttempts = %d, want 8", p.MaxAttempts())
	}
	if p.Schedule[0] != 0 {
		t.Fatalf("the first attempt must be immediate, got %v", p.Schedule[0])
	}
	var total time.Duration
	for i := 1; i < len(p.Schedule); i++ {
		if p.Schedule[i] < p.Schedule[i-1] {
			t.Fatalf("schedule must not shrink: %v then %v", p.Schedule[i-1], p.Schedule[i])
		}
		total += p.Schedule[i]
	}
	if total < 24*time.Hour || total > 30*time.Hour {
		t.Fatalf("a delivery should be retried for about a day, got %v", total)
	}
}

func TestNextWalksTheScheduleThenStops(t *testing.T) {
	p := Policy{Schedule: []time.Duration{0, time.Second, time.Minute}}
	for made, want := range []time.Duration{0, time.Second, time.Minute} {
		got, ok := p.Next(made, fixed(0.5), 0)
		if !ok || got != want {
			t.Fatalf("Next(%d) = %v, %v; want %v, true", made, got, ok, want)
		}
	}
	if _, ok := p.Next(3, fixed(0.5), 0); ok {
		t.Fatal("no attempt should be allowed after the schedule is used up")
	}
	if _, ok := p.Next(-1, fixed(0.5), 0); ok {
		t.Fatal("a negative attempt count is not valid")
	}
}

func TestJitterStaysInsideItsBand(t *testing.T) {
	p := Policy{Schedule: []time.Duration{0, 100 * time.Second}, Jitter: 0.2}
	cases := []struct {
		rnd  float64
		want time.Duration
	}{
		{0.0, 80 * time.Second},
		{0.5, 100 * time.Second},
		{0.999999, 120 * time.Second},
	}
	for _, c := range cases {
		got, _ := p.Next(1, fixed(c.rnd), 0)
		if diff := got - c.want; diff < -time.Millisecond*200 || diff > time.Millisecond*200 {
			t.Errorf("rnd %v: got %v, want about %v", c.rnd, got, c.want)
		}
	}
}

func TestImmediateAttemptIsNeverDelayedByJitter(t *testing.T) {
	p := Policy{Schedule: []time.Duration{0, time.Second}, Jitter: 0.5}
	if got, _ := p.Next(0, fixed(0.99), 0); got != 0 {
		t.Fatalf("an immediate attempt must stay immediate, got %v", got)
	}
}

func TestRetryAfterWinsWhenLongerButIsCapped(t *testing.T) {
	p := Policy{Schedule: []time.Duration{0, 5 * time.Second}}
	if got, _ := p.Next(1, fixed(0.5), time.Minute); got != time.Minute {
		t.Errorf("a longer Retry-After should win, got %v", got)
	}
	if got, _ := p.Next(1, fixed(0.5), time.Second); got != 5*time.Second {
		t.Errorf("a shorter Retry-After must not shorten the wait, got %v", got)
	}
	if got, _ := p.Next(1, fixed(0.5), 100*24*time.Hour); got != MaxRetryAfter {
		t.Errorf("Retry-After must be capped at %v, got %v", MaxRetryAfter, got)
	}
}

func TestRetryAfterIsNotJittered(t *testing.T) {
	p := Policy{Schedule: []time.Duration{0, time.Second}, Jitter: 0.5}
	if got, _ := p.Next(1, fixed(0.99), 10*time.Minute); got != 10*time.Minute {
		t.Errorf("a receiver's Retry-After is taken as given, got %v", got)
	}
}
