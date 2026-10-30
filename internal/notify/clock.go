package notify

import (
	"context"
	"time"
)

// Clock is the engine's only source of time, injected so that cooldowns, TTLs,
// backoff, rate limiting and the monitoring-down timer are testable without
// sleeping.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives once d has elapsed on this clock.
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// sleep waits d on c or until ctx ends.
func sleep(ctx context.Context, c Clock, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	select {
	case <-c.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
