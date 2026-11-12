// Package retry decides when a failed delivery is tried again.
//
// The policy is a plain list of waits, with random jitter so that many deliveries that failed together do not all
// come back at the same instant. It has no clock and no random source of its own, which keeps it easy to test.
package retry

import "time"

// DefaultSchedule is the wait before each attempt. The first attempt is immediate, the last one comes about 27
// hours after the first. It follows the schedule suggested by the Standard Webhooks specification.
var DefaultSchedule = []time.Duration{
	0,
	5 * time.Second,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	5 * time.Hour,
	10 * time.Hour,
	10 * time.Hour,
}

// MaxRetryAfter caps how long a receiver can ask us to wait with a Retry-After header. Without a cap a receiver
// could park a delivery for years.
const MaxRetryAfter = 24 * time.Hour

// Policy is a retry schedule and how much to randomize it.
type Policy struct {
	// Schedule[i] is the wait before attempt i+1. Schedule[0] is normally zero.
	Schedule []time.Duration
	// Jitter is the fraction of each wait that may be added or removed, from 0 to 1. 0.2 means plus or minus 20%.
	Jitter float64
}

// Default returns the standard policy with 20% jitter.
func Default() Policy {
	return Policy{Schedule: DefaultSchedule, Jitter: 0.2}
}

// MaxAttempts is the total number of attempts the policy allows, the first one included.
func (p Policy) MaxAttempts() int { return len(p.Schedule) }

// Next returns the wait before the next attempt, given how many attempts have already been made.
// ok is false when the attempts are used up and the delivery should be given up on.
//
// rnd returns a number in [0, 1). retryAfter is the wait the receiver asked for, or zero. When it is longer than
// the schedule's wait it wins, up to MaxRetryAfter, and it is not jittered because the receiver chose it.
func (p Policy) Next(attemptsMade int, rnd func() float64, retryAfter time.Duration) (wait time.Duration, ok bool) {
	if attemptsMade < 0 || attemptsMade >= len(p.Schedule) {
		return 0, false
	}
	wait = p.Schedule[attemptsMade]
	if p.Jitter > 0 && wait > 0 {
		// A factor in [1-Jitter, 1+Jitter].
		factor := 1 + p.Jitter*(2*rnd()-1)
		wait = time.Duration(float64(wait) * factor)
	}
	if retryAfter > MaxRetryAfter {
		retryAfter = MaxRetryAfter
	}
	if retryAfter > wait {
		wait = retryAfter
	}
	return wait, true
}
