package notify

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Sender delivers one message to a transport. Implementations must honour ctx.
// The Discord implementation belongs to a later milestone; this package never
// imports a transport.
//
// A Sender reports failures as a *SendError (use Retryable, RateLimited and
// Permanent). Any other error is treated as retryable, because a timeout or a
// dropped connection is ambiguous: the transport may have accepted the message
// even though no success came back. Retrying such a failure can therefore
// deliver a message twice. That is accepted: delivery is best effort with
// possible duplicates, and a missed leak alert is judged worse than a duplicate.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// FailureKind classifies a failed send.
type FailureKind uint8

const (
	// FailureRetryable: network error, ambiguous timeout, server error. Retried with backoff until the TTL.
	FailureRetryable FailureKind = iota + 1
	// FailureRateLimited: the transport asked us to wait. Retried after RetryAfter.
	FailureRateLimited
	// FailurePermanent: authorization or invalid channel. Never retried.
	FailurePermanent
)

// SendError is a classified send failure.
type SendError struct {
	Kind       FailureKind
	RetryAfter time.Duration
	Err        error
}

func (e *SendError) Error() string {
	if e.Err == nil {
		return "notification send failed"
	}
	return "notification send failed: " + e.Err.Error()
}

func (e *SendError) Unwrap() error { return e.Err }

// Retryable marks err as retryable.
func Retryable(err error) error { return &SendError{Kind: FailureRetryable, Err: err} }

// RateLimited marks err as a rate limit that clears after the given time.
func RateLimited(err error, after time.Duration) error {
	return &SendError{Kind: FailureRateLimited, RetryAfter: after, Err: err}
}

// Permanent marks err as not worth retrying.
func Permanent(err error) error { return &SendError{Kind: FailurePermanent, Err: err} }

func classify(err error) (FailureKind, time.Duration) {
	var se *SendError
	if errors.As(err, &se) {
		return se.Kind, se.RetryAfter
	}
	return FailureRetryable, 0
}

// retryBackoff is an exponential backoff with equal jitter: half the delay is
// fixed and half is random. rand(n) returns a value in [0, n); tests inject a
// deterministic one.
type retryBackoff struct {
	initial, max time.Duration
	rand         func(n int64) int64
}

// delay returns the wait before retry number attempt (1 is the first retry).
func (b retryBackoff) delay(attempt int) time.Duration {
	d := b.initial
	for i := 1; i < attempt && d < b.max; i++ {
		d *= 2
	}
	if d > b.max {
		d = b.max
	}
	half := d / 2
	return half + time.Duration(b.rand(int64(half)+1))
}

// tokenBucket limits the send rate. wait reserves a token and says how long the
// caller must wait before using it.
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
	primed bool
}

func newTokenBucket(rate float64, burst int) *tokenBucket {
	return &tokenBucket{rate: rate, burst: float64(burst), tokens: float64(burst)}
}

func (b *tokenBucket) wait(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.primed {
		b.tokens += b.rate * now.Sub(b.last).Seconds()
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
	}
	b.last, b.primed = now, true
	b.tokens--
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / b.rate * float64(time.Second))
}

// breaker opens after a run of permanent failures (a broken channel or token)
// and then lets one probe through per interval, so a repaired channel recovers
// by itself without hammering a broken one.
type breaker struct {
	mu        sync.Mutex
	threshold int
	probe     time.Duration
	fails     int
	open      bool
	openUntil time.Time
}

// allow reports whether a send may be attempted now. While open it admits a
// single probe once the interval has passed.
func (b *breaker) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return true
	}
	if now.Before(b.openUntil) {
		return false
	}
	b.openUntil = now.Add(b.probe) // the probe in flight blocks the others
	return true
}

func (b *breaker) success() (wasOpen bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	wasOpen = b.open
	b.fails, b.open = 0, false
	return wasOpen
}

func (b *breaker) permanentFailure(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails++
	if b.open || b.fails >= b.threshold {
		b.open, b.openUntil = true, now.Add(b.probe)
	}
}

func (b *breaker) isOpen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.open
}

// counters are the engine's monotonic totals. Every message that does not reach
// the transport is counted under exactly one reason.
type counters struct {
	enqueued, delivered, retried                                                      atomic.Uint64
	droppedOverflow, droppedTTL, droppedRevoked, droppedPermanent, droppedBreaker     atomic.Uint64
	droppedDisabled, droppedShutdown, droppedCancelled                                atomic.Uint64
	suppressedCooldown, suppressedUnshared                                            atomic.Uint64
	abandoned, digests                                                                atomic.Uint64
	episodesStarted, episodesFired, episodesCancelled                                 atomic.Uint64
	initialAlerts, markSuppressed, startupListed, startupSummaries, summarySuppressed atomic.Uint64
	lastDropNanos                                                                     atomic.Int64
}

func (c *counters) countDrop(r dropReason, at time.Time) {
	switch r {
	case dropOverflow:
		c.droppedOverflow.Add(1)
	case dropTTL:
		c.droppedTTL.Add(1)
	case dropRevoked:
		c.droppedRevoked.Add(1)
	case dropPermanent:
		c.droppedPermanent.Add(1)
	case dropBreaker:
		c.droppedBreaker.Add(1)
	case dropDisabled:
		c.droppedDisabled.Add(1)
	case dropShutdown:
		c.droppedShutdown.Add(1)
	case dropCancelled:
		c.droppedCancelled.Add(1)
	}
	if r != dropRevoked && r != dropDisabled && r != dropShutdown && r != dropCancelled {
		c.lastDropNanos.Store(at.UnixNano()) // a drop that lost an alert degrades health
	}
}
