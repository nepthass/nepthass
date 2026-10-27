package discord

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

var quickBackoff = backoff{initial: time.Millisecond, max: 4 * time.Millisecond}

func TestBackoff_retriesTransientFailuresUntilSuccess(t *testing.T) {
	calls := 0

	err := quickBackoff.retry(context.Background(), slog.New(slog.DiscardHandler), "test", func() error {
		calls++
		if calls < 4 {
			return errors.New("network down")
		}
		return nil
	})

	if err != nil || calls != 4 {
		t.Errorf("err=%v calls=%d, want nil and 4", err, calls)
	}
}

func TestBackoff_stopsImmediately_onPermanentError(t *testing.T) {
	calls := 0

	err := quickBackoff.retry(context.Background(), slog.New(slog.DiscardHandler), "test", func() error {
		calls++
		return permanentError{err: ErrInvalidToken}
	})

	if !errors.Is(err, ErrInvalidToken) || calls != 1 {
		t.Errorf("err=%v calls=%d, want ErrInvalidToken and 1", err, calls)
	}
}

func TestBackoff_stopsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0

	err := quickBackoff.retry(ctx, slog.New(slog.DiscardHandler), "test", func() error {
		calls++
		cancel()
		return errors.New("still failing")
	})

	if err == nil || calls != 1 {
		t.Errorf("err=%v calls=%d, want an error after exactly one call", err, calls)
	}
}
