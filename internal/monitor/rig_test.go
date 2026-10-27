package monitor

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/config"
	"github.com/nepthass/nepthass/internal/homeassistant/ws"
	"github.com/nepthass/nepthass/tests/fakeha"
)

const (
	testToken   = "test-token"
	waitTimeout = 10 * time.Second
)

var (
	tempAttrs = map[string]any{"device_class": "temperature", "unit_of_measurement": "\u00b0C", "friendly_name": "Temp"}
	doorAttrs = map[string]any{"device_class": "door", "friendly_name": "Door"}
)

// seed gives the fake four monitored sensors and two entities that must not be tracked.
func seed(s *fakeha.Server) {
	s.Set("sensor.temp", "21.5", tempAttrs)
	s.Set("sensor.humidity", "45", map[string]any{"device_class": "humidity", "unit_of_measurement": "%"})
	s.Set("binary_sensor.door", "off", doorAttrs)
	s.Set("binary_sensor.motion", "off", map[string]any{"device_class": "motion"})
	s.Set("light.kitchen", "on", nil)  // not a monitored domain
	s.Set("sensor.uptime", "123", nil) // monitored domain, no device class: not classified
}

const seededSensors = 4

// recSleep records the backoff delays the monitor asks for. The first
// "immediate" waits return at once; later ones block until the context ends,
// so a test can assert an exact schedule without any real waiting.
type recSleep struct {
	mu        sync.Mutex
	delays    []time.Duration
	immediate int
	changed   chan struct{}
}

func newRecSleep(immediate int) *recSleep {
	return &recSleep{immediate: immediate, changed: make(chan struct{})}
}

func (r *recSleep) sleep(ctx context.Context, d time.Duration) error {
	r.mu.Lock()
	r.delays = append(r.delays, d)
	instant := len(r.delays) <= r.immediate
	close(r.changed)
	r.changed = make(chan struct{})
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if instant {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

// waitFor blocks until at least n delays were requested and returns a copy.
func (r *recSleep) waitFor(t *testing.T, n int) []time.Duration {
	t.Helper()
	deadline := time.After(waitTimeout)
	for {
		r.mu.Lock()
		got, ch := append([]time.Duration(nil), r.delays...), r.changed
		r.mu.Unlock()
		if len(got) >= n {
			return got
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("only %d of %d backoff waits happened", len(got), n)
		}
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// rig is a Monitor wired to a fake Home Assistant with millisecond backoff.
type rig struct {
	t      *testing.T
	srv    *fakeha.Server
	m      *Monitor
	cancel context.CancelFunc
	ctx    context.Context
}

func newRig(t *testing.T, mod func(*Config)) *rig {
	t.Helper()
	srv := fakeha.New(t, testToken)
	seed(srv)
	cfg := Config{
		HomeAssistant: config.HomeAssistant{URL: srv.URL, Token: config.Secret(testToken), Timeout: time.Second},
		Backoff: Backoff{
			Initial: 2 * time.Millisecond, Max: 16 * time.Millisecond, AuthInitial: 20 * time.Millisecond, AuthMax: 80 * time.Millisecond,
			StableAfter: time.Hour, Rand: func(n int64) int64 { return n - 1 }, // full delay: deterministic
		},
		Session: ws.Options{HandshakeTimeout: time.Second, WriteTimeout: time.Second, ReadTimeout: time.Minute, PingInterval: time.Hour},
	}
	if mod != nil {
		mod(&cfg)
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, srv: srv, m: m}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	t.Cleanup(r.shutdown)
	return r
}

func (r *rig) start() *rig {
	go r.m.Run(r.ctx)
	return r
}

// shutdown stops the monitor and proves nothing leaked: Run returned, no
// illegal transition happened, and the server sees no open connection.
func (r *rig) shutdown() {
	r.t.Helper()
	r.cancel()
	r.srv.ReleaseStates()
	if r.m.running.Load() {
		select {
		case <-r.m.Done():
		case <-time.After(waitTimeout):
			r.t.Error("Run did not return after shutdown")
			return
		}
	}
	wctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := r.srv.WaitWS(wctx, func(s fakeha.WSStats) bool { return s.Active == 0 }); err != nil {
		r.t.Errorf("a WebSocket connection leaked: %v", err)
	}
	if v := r.m.Violations(); v != 0 {
		r.t.Errorf("%d illegal state transitions", v)
	}
}

func (r *rig) wait(pred func(Status) bool) Status {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := r.m.WaitFor(ctx, pred); err != nil {
		r.t.Fatalf("condition not reached (%v); status = %+v", err, r.m.Status())
	}
	return r.m.Status()
}

func (r *rig) waitReady() Status {
	r.t.Helper()
	return r.wait(func(s Status) bool { return s.State == StateReady && s.Healthy })
}

func (r *rig) waitSensor(id, raw string) {
	r.t.Helper()
	r.wait(func(Status) bool {
		s, ok := r.m.Tracker().Get(entityID(id))
		return ok && s.Raw == raw
	})
}
