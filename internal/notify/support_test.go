package notify

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
)

const testWait = 10 * time.Second

// fakeClock is a manually advanced Clock. Tests wait for goroutines to reach a
// sleep with AwaitWaiters instead of sleeping themselves.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*clockWaiter
	changed chan struct{}
}

type clockWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), changed: make(chan struct{})}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := &clockWaiter{at: c.now.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		w.ch <- c.now
		return w.ch
	}
	c.waiters = append(c.waiters, w)
	close(c.changed)
	c.changed = make(chan struct{})
	return w.ch
}

// Advance moves time forward and releases every sleeper whose time has come.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			kept = append(kept, w)
		}
	}
	c.waiters = kept
}

// AwaitWaiters blocks until at least n goroutines are sleeping on the clock.
func (c *fakeClock) AwaitWaiters(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(testWait)
	for {
		c.mu.Lock()
		got, ch := len(c.waiters), c.changed
		c.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("only %d of %d goroutines are sleeping on the clock", got, n)
		}
	}
}

// fakeSender records messages and can fail or block on script.
type fakeSender struct {
	mu       sync.Mutex
	calls    []Message
	script   func(n int, m Message) error
	gate     chan struct{}
	honorCtx bool
	changed  chan struct{}
}

func newFakeSender() *fakeSender { return &fakeSender{changed: make(chan struct{})} }

func (s *fakeSender) Send(ctx context.Context, m Message) error {
	s.mu.Lock()
	s.calls = append(s.calls, m)
	n, gate, script, honor := len(s.calls), s.gate, s.script, s.honorCtx
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
	if gate != nil {
		if honor {
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		} else {
			<-gate
		}
	}
	if script != nil {
		return script(n, m)
	}
	return nil
}

func (s *fakeSender) messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.calls...)
}

func (s *fakeSender) waitCalls(t *testing.T, n int) []Message {
	t.Helper()
	deadline := time.After(testWait)
	for {
		s.mu.Lock()
		got, ch := append([]Message(nil), s.calls...), s.changed
		s.mu.Unlock()
		if len(got) >= n {
			return got
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("only %d of %d sends happened", len(got), n)
		}
	}
}

type fakeStatus struct {
	mu sync.Mutex
	st monitor.Status
}

func (f *fakeStatus) Status() monitor.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *fakeStatus) set(healthy bool, state monitor.ConnState) {
	f.mu.Lock()
	f.st = monitor.Status{State: state, Healthy: healthy}
	f.mu.Unlock()
}

// flakyAccess wraps a real AccessSource and can fail on demand.
type flakyAccess struct {
	inner AccessSource
	mu    sync.Mutex
	fail  bool
}

func (a *flakyAccess) Get(ctx context.Context, id entities.ID) (entities.Access, error) {
	a.mu.Lock()
	fail := a.fail
	a.mu.Unlock()
	if fail {
		return entities.AccessNone, context.DeadlineExceeded
	}
	return a.inner.Get(ctx, id)
}

func (a *flakyAccess) setFail(v bool) {
	a.mu.Lock()
	a.fail = v
	a.mu.Unlock()
}

func sens(cat monitor.Category, raw string) monitor.Sensor {
	return monitor.Sensor{ID: "x.sensor", Category: cat, Name: "Name", Raw: raw, Unit: "u", LastChanged: time.Unix(1_000_000, 0).UTC()}
}

func chg(id string, cat monitor.Category, hadPrev bool, prev, cur string, src monitor.ChangeSource, seq uint64) monitor.Change {
	c := monitor.Change{ID: entities.ID(id), Seq: seq, Source: src, HadPrev: hadPrev, At: time.Unix(1_000_100, 0).UTC()}
	c.Cur = sens(cat, cur)
	c.Cur.ID = c.ID
	if hadPrev {
		c.Prev = sens(cat, prev)
		c.Prev.ID = c.ID
	}
	return c
}

func rule(id int64, kind Kind, thr float64) cachedRule {
	return cachedRule{ID: id, Entity: "x.sensor", Kind: kind, Threshold: thr}
}
