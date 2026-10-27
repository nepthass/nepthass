package monitor

import "time"

// Backoff computes retry delays: exponential growth to a maximum with equal
// jitter (a delay is uniformly spread over its upper half, so reconnecting
// clients never synchronize). Credential failures use a much slower schedule
// so a revoked token never becomes a hot loop.
type Backoff struct {
	Initial     time.Duration
	Max         time.Duration
	AuthInitial time.Duration
	AuthMax     time.Duration
	// StableAfter is how long a session must stay Ready before the schedule
	// resets. A server that accepts and immediately drops us keeps the delay
	// growing instead of producing a reconnect storm.
	StableAfter time.Duration
	// Rand returns a uniform integer in [0, n). Injected for deterministic tests.
	Rand func(n int64) int64

	attempts     int
	authAttempts int
}

// DefaultBackoff is the production schedule.
func DefaultBackoff() Backoff {
	return Backoff{
		Initial: time.Second, Max: time.Minute,
		AuthInitial: 30 * time.Second, AuthMax: 10 * time.Minute,
		StableAfter: 30 * time.Second,
	}
}

// Next returns the delay before the next attempt after a failure of the given
// class and advances the schedule.
func (b *Backoff) Next(class Failure) time.Duration {
	initial, limit, attempt := b.Initial, b.Max, &b.attempts
	if class == FailureAuthentication {
		initial, limit, attempt = b.AuthInitial, b.AuthMax, &b.authAttempts
	}
	d := grow(initial, limit, *attempt)
	*attempt++
	return b.jitter(d)
}

// Reset restarts both schedules (a session proved stable).
func (b *Backoff) Reset() { b.attempts, b.authAttempts = 0, 0 }

// Attempts reports how many failures the transient schedule has seen.
func (b *Backoff) Attempts() int { return b.attempts }

// grow is initial * 2^attempt capped at limit, without overflowing.
func grow(initial, limit time.Duration, attempt int) time.Duration {
	d := initial
	for range attempt {
		if d >= limit/2 {
			return limit
		}
		d *= 2
	}
	return min(d, limit)
}

// jitter spreads d over [d/2, d].
func (b *Backoff) jitter(d time.Duration) time.Duration {
	half := d / 2
	if b.Rand == nil || half <= 0 {
		return d
	}
	return half + time.Duration(b.Rand(int64(half)+1))
}
