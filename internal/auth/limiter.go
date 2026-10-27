package auth

import (
	"sync"
	"time"
)

// Limiter is a sliding-window rate limiter keyed by user ID. Call it only for
// users that already passed Authorizer.Authorize so the key space stays
// bounded by the allowlists.
type Limiter struct {
	mu     sync.Mutex
	now    func() time.Time
	limit  int
	window time.Duration
	hits   map[string][]time.Time
}

// NewLimiter allows at most limit calls per key within window. now is injected for deterministic tests.
func NewLimiter(limit int, window time.Duration, now func() time.Time) *Limiter {
	return &Limiter{now: now, limit: limit, window: window, hits: make(map[string][]time.Time)}
}

// Allow records an attempt and reports whether it is within the limit.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-l.window)
	recent := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= l.limit {
		l.hits[key] = recent
		return false
	}
	l.hits[key] = append(recent, now)
	return true
}
