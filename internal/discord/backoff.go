package discord

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"
)

// permanentError marks a failure that retrying cannot fix (for example a rejected bot token).
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// backoff retries an operation with exponential delay and jitter.
type backoff struct {
	initial time.Duration
	max     time.Duration
}

// retry runs fn until it succeeds, returns a permanentError, or ctx ends.
// Delays double up to b.max and each wait is randomized between half and the
// full delay so that restarts of several clients do not synchronize.
func (b backoff) retry(ctx context.Context, log *slog.Logger, what string, fn func() error) error {
	delay := b.initial
	for {
		err := fn()
		if err == nil {
			return nil
		}
		var permanent permanentError
		if errors.As(err, &permanent) || ctx.Err() != nil {
			return err
		}
		wait := delay/2 + rand.N(delay/2+1)
		log.Warn("operation failed, will retry", "operation", what, "error", err, "retry_in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		delay = min(delay*2, b.max)
	}
}
