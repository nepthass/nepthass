package monitor

import (
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/config"
	"github.com/nepthass/nepthass/internal/homeassistant/ws"
	"github.com/nepthass/nepthass/tests/fakeha"
)

// pingingRig keeps the connection alive with fast pings that the fake answers, so
// only the command timeout can end an attempt (the read timeout is far away).
func pingingRig(t *testing.T) *rig {
	return newRig(t, func(c *Config) {
		c.Session = ws.Options{HandshakeTimeout: time.Second, PingInterval: 20 * time.Millisecond, PongTimeout: time.Second, ReadTimeout: time.Minute}
		c.CommandTimeout, c.SnapshotTimeout = 100*time.Millisecond, 100*time.Millisecond
	})
}

func TestFailure_subscribeNeverAnswered_whilePingsAreAnswered_timesOutAndReconnects(t *testing.T) {
	r := pingingRig(t)
	r.srv.Script(func(sc *fakeha.Script) { sc.SilentSubscribe = true })
	r.start()

	stuck := r.wait(func(s Status) bool { return s.Reconnects >= 2 })
	during := r.srv.WSStats()
	r.srv.Script(func(sc *fakeha.Script) { sc.SilentSubscribe = false })
	ready := r.waitReady()

	if stuck.LastFailure != FailureNetwork || stuck.Healthy || stuck.Subscribed {
		t.Errorf("an unanswered subscription must end the attempt as a failure: %+v", stuck)
	}
	if during.Pings < 2 || during.Subscribed != 0 {
		t.Errorf("server stats = %+v: the peer must have been answering pings while it ignored the subscription, which is the case that used to hang forever", during)
	}
	if ready.Tracked != seededSensors || r.srv.WSStats().MaxSubscribed != 1 {
		t.Errorf("recovery: %+v stats=%+v", ready, r.srv.WSStats())
	}
}

func TestFailure_snapshotNeverAnswered_whilePingsAreAnswered_timesOutReconnectsAndNeverDuplicatesSubscriptions(t *testing.T) {
	r := pingingRig(t)
	r.srv.Script(func(sc *fakeha.Script) { sc.SilentStates = true })
	r.start()

	stuck := r.wait(func(s Status) bool { return s.Reconnects >= 2 })
	during := r.srv.WSStats()
	r.srv.Script(func(sc *fakeha.Script) { sc.SilentStates = false })
	ready := r.waitReady()

	if stuck.LastFailure != FailureNetwork || stuck.Healthy || stuck.Synchronized {
		t.Errorf("an unanswered snapshot must end the attempt: %+v", stuck)
	}
	if during.StateRequests < 2 || during.Pings < 2 {
		t.Errorf("server stats = %+v: the snapshot must have been requested on each attempt while pings were answered", during)
	}
	if s := r.srv.WSStats(); s.MaxSubscribed != 1 || ready.Tracked != seededSensors {
		t.Errorf("a timed-out attempt must release its subscription before the next one: stats=%+v ready=%+v", s, ready)
	}
}

func TestFailure_shutdownWhileWaitingForACommand_isImmediate_andNotAFailure(t *testing.T) {
	for _, tt := range []struct {
		name   string
		script func(*fakeha.Script)
		state  ConnState
	}{
		{"subscribe_events", func(sc *fakeha.Script) { sc.SilentSubscribe = true }, StateSubscribing},
		{"get_states", func(sc *fakeha.Script) { sc.SilentStates = true }, StateSynchronizing},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, func(c *Config) { c.CommandTimeout, c.SnapshotTimeout = time.Hour, time.Hour })
			r.srv.Script(tt.script)
			r.start()
			r.wait(func(s Status) bool { return s.State == tt.state })

			r.cancel()

			select {
			case <-r.m.Done():
			case <-time.After(waitTimeout):
				t.Fatal("Run kept waiting for an answer after the application was told to stop")
			}
			if st := r.m.Status(); st.State != StateStopped || st.Reconnects != 0 {
				t.Errorf("shutdown while waiting must stop cleanly and must not count as a failure: %+v", st)
			}
			// rig.shutdown (registered cleanup) additionally proves the server sees
			// no open connection and that no illegal state transition happened.
		})
	}
}

func TestNew_commandTimeoutsAreBoundedByDefault(t *testing.T) {
	m, err := New(Config{HomeAssistant: config.HomeAssistant{URL: "http://127.0.0.1:1", Token: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	negative, err := New(Config{HomeAssistant: config.HomeAssistant{URL: "http://127.0.0.1:1", Token: "x"}, CommandTimeout: -1, SnapshotTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}

	for name, got := range map[string]*Monitor{"zero": m, "negative": negative} {
		if got.cfg.CommandTimeout != DefaultCommandTimeout || got.cfg.SnapshotTimeout != DefaultSnapshotTimeout {
			t.Errorf("%s: timeouts = %v / %v, want the bounded defaults", name, got.cfg.CommandTimeout, got.cfg.SnapshotTimeout)
		}
	}
	if DefaultCommandTimeout <= 0 || DefaultSnapshotTimeout < DefaultCommandTimeout {
		t.Errorf("defaults must be positive and the snapshot allowance at least the command one: %v %v", DefaultCommandTimeout, DefaultSnapshotTimeout)
	}
}
