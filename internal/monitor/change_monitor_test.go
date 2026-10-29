package monitor

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/tests/fakeha"
)

// changeRecorder collects what OnChange delivers. Tests wait on it, never on time.
type changeRecorder struct {
	mu      sync.Mutex
	got     []Change
	changed chan struct{}
}

func newChangeRecorder() *changeRecorder { return &changeRecorder{changed: make(chan struct{})} }

func (c *changeRecorder) record(ch Change) {
	c.mu.Lock()
	c.got = append(c.got, ch)
	close(c.changed)
	c.changed = make(chan struct{})
	c.mu.Unlock()
}

func (c *changeRecorder) all() []Change {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.got)
}

// waitLen blocks until at least n changes were delivered and returns them.
func (c *changeRecorder) waitLen(t *testing.T, n int) []Change {
	t.Helper()
	deadline := time.After(waitTimeout)
	for {
		c.mu.Lock()
		got, ch := slices.Clone(c.got), c.changed
		c.mu.Unlock()
		if len(got) >= n {
			return got
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("only %d of %d changes were delivered", len(got), n)
		}
	}
}

func sources(cs []Change) []ChangeSource {
	out := make([]ChangeSource, len(cs))
	for i, c := range cs {
		out[i] = c.Source
	}
	return out
}

func assertStrictlyIncreasing(t *testing.T, cs []Change) {
	t.Helper()
	for i, c := range cs {
		if c.Seq != uint64(i+1) {
			t.Fatalf("change %d has Seq %d: sequence numbers must be 1,2,3,... in delivery order (%v)", i, c.Seq, seqs(cs))
		}
	}
}

func seqs(cs []Change) []uint64 {
	out := make([]uint64, len(cs))
	for i, c := range cs {
		out[i] = c.Seq
	}
	return out
}

func TestOnChange_theFirstSynchronizationIsAllBaseline(t *testing.T) {
	rec := newChangeRecorder()
	r := newRig(t, func(c *Config) { c.OnChange = rec.record }).start()
	r.waitReady()

	got := rec.all()
	if len(got) != seededSensors {
		t.Fatalf("expected one change per tracked sensor, got %d: %+v", len(got), got)
	}
	assertStrictlyIncreasing(t, got)
	seen := map[entities.ID]bool{}
	for _, c := range got {
		if c.Source != SourceSnapshot || c.HadPrev || c.Prev != (Sensor{}) || c.Removed || c.Cur.Revision != 1 {
			t.Errorf("the first synchronization must report baselines only: %+v", c)
		}
		seen[c.ID] = true
	}
	if len(seen) != seededSensors {
		t.Errorf("every sensor exactly once: %v", seen)
	}
	if r.m.HookPanics() != 0 {
		t.Errorf("hook panics = %d", r.m.HookPanics())
	}
}

func TestOnChange_snapshotThenReplayThenLive_inDeliveryOrder(t *testing.T) {
	rec := newChangeRecorder()
	r := newRig(t, func(c *Config) { c.OnChange = rec.record })
	r.srv.Script(func(sc *fakeha.Script) { sc.HoldStates = true })
	r.start()
	if err := r.srv.WaitWS(r.ctx, func(s fakeha.WSStats) bool { return s.StateRequests == 1 }); err != nil {
		t.Fatal(err)
	}

	// The snapshot was captured when get_states arrived. These events travel on
	// the same ordered connection and are buffered before the reply is delivered.
	stale := fakeha.StateObject("sensor.temp", "99", tempAttrs, base.Add(-time.Hour), base.Add(-time.Hour))
	r.srv.Update("binary_sensor.door", "on", doorAttrs)                             // newer than the snapshot: replay
	r.srv.EmitStateChanged("sensor.temp", nil, stale)                               // older than the snapshot: silent
	r.srv.EmitStateChanged("sensor.humidity", nil, r.srv.Object("sensor.humidity")) // identical to the snapshot: silent
	r.srv.Update("binary_sensor.motion", "on", map[string]any{"device_class": "motion"})
	r.srv.ReleaseStates()
	st := r.waitReady()

	r.srv.Update("sensor.temp", "22.0", tempAttrs)
	got := rec.waitLen(t, seededSensors+2+1)

	assertStrictlyIncreasing(t, got)
	want := []ChangeSource{SourceSnapshot, SourceSnapshot, SourceSnapshot, SourceSnapshot, SourceReplay, SourceReplay, SourceLive}
	if !slices.Equal(sources(got), want) {
		t.Fatalf("sources = %v, want %v", sources(got), want)
	}
	door, motion, live := got[4], got[5], got[6]
	if door.ID != "binary_sensor.door" || !door.HadPrev || door.Prev.Raw != "off" || door.Cur.Raw != "on" {
		t.Errorf("a replayed change must see the reconciled snapshot as its previous state: %+v", door)
	}
	if motion.ID != "binary_sensor.motion" {
		t.Errorf("replay keeps the order Home Assistant delivered: door then motion, got %s then %s", door.ID, motion.ID)
	}
	if live.ID != "sensor.temp" || !live.HadPrev || live.Prev.Raw != "21.5" || live.Cur.Raw != "22.0" {
		t.Errorf("live change: %+v", live)
	}
	if st.StaleEvents < 1 || st.DuplicateEvents < 1 {
		t.Errorf("the stale and duplicate events were processed (and counted) but must be silent: %+v", st)
	}
	if len(rec.all()) != seededSensors+2+1 {
		t.Errorf("stale and duplicate updates must not produce changes: %d changes", len(rec.all()))
	}
}

func TestOnChange_manyLiveEventsArriveInOrderWithoutGaps(t *testing.T) {
	rec := newChangeRecorder()
	r := newRig(t, func(c *Config) { c.OnChange = rec.record }).start()
	r.waitReady()

	const n = 50
	for i := 0; i < n; i++ {
		r.srv.Update("sensor.temp", fmt.Sprintf("%d", 100+i), tempAttrs)
	}
	got := rec.waitLen(t, seededSensors+n)

	assertStrictlyIncreasing(t, got)
	for i := 0; i < n; i++ {
		c := got[seededSensors+i]
		if c.Source != SourceLive || c.Cur.Raw != fmt.Sprintf("%d", 100+i) || c.Cur.Revision != uint64(i+2) {
			t.Fatalf("live change %d out of order or lost: %+v", i, c)
		}
		if i > 0 && c.Prev.Raw != got[seededSensors+i-1].Cur.Raw {
			t.Fatalf("change %d does not continue from the previous one: prev=%q", i, c.Prev.Raw)
		}
	}
}

func TestOnChange_aReconnectReportsOnlyWhatChangedAsSnapshotChangesWithPrevious(t *testing.T) {
	rec := newChangeRecorder()
	r := newRig(t, func(c *Config) { c.OnChange = rec.record }).start()
	r.waitReady()
	rec.waitLen(t, seededSensors)

	r.srv.Down()
	r.wait(func(s Status) bool { return s.State == StateReconnecting && s.Reconnects >= 1 })
	// Nobody is connected: these changes are invisible until the next snapshot.
	r.srv.Update("binary_sensor.door", "on", doorAttrs)
	r.srv.Remove("sensor.humidity")
	r.srv.Up()

	got := rec.waitLen(t, seededSensors+2)
	r.waitReady()
	got = rec.all()
	assertStrictlyIncreasing(t, got)
	if len(got) != seededSensors+2 {
		t.Fatalf("unchanged sensors must not be reported again: %d changes", len(got))
	}
	door, humidity := got[seededSensors], got[seededSensors+1]
	if door.Source != SourceSnapshot || door.ID != "binary_sensor.door" || !door.HadPrev || door.Prev.Raw != "off" || door.Cur.Raw != "on" {
		t.Errorf("a transition found by the snapshot after a reconnect keeps its previous state: %+v", door)
	}
	if humidity.Source != SourceSnapshot || humidity.ID != "sensor.humidity" || !humidity.Removed || !humidity.HadPrev || humidity.Prev.Raw != "45" {
		t.Errorf("a removal found by the snapshot: %+v", humidity)
	}
}

func TestOnChange_theCallbackCanReadTheTrackerWithoutDeadlock(t *testing.T) {
	var mismatches, reads int
	var mu sync.Mutex
	var r *rig
	rec := newChangeRecorder()
	r = newRig(t, func(c *Config) {
		c.OnChange = func(ch Change) {
			tr := r.m.Tracker()
			s, ok := tr.Get(ch.ID) // would deadlock if the tracker lock were held
			_, _ = tr.Len(), len(tr.All())
			mu.Lock()
			reads++
			if ch.Removed == ok || (ok && s != ch.Cur) {
				mismatches++
			}
			mu.Unlock()
			rec.record(ch)
		}
	})
	r.srv.Script(func(sc *fakeha.Script) { sc.HoldStates = true })
	r.start()
	if err := r.srv.WaitWS(r.ctx, func(s fakeha.WSStats) bool { return s.StateRequests == 1 }); err != nil {
		t.Fatal(err)
	}
	r.srv.Update("binary_sensor.door", "on", doorAttrs) // replay path
	r.srv.ReleaseStates()
	r.waitReady()
	r.srv.Update("sensor.temp", "23.0", tempAttrs) // live path
	r.srv.Remove("sensor.humidity")                // removal path
	rec.waitLen(t, seededSensors+3)

	mu.Lock()
	defer mu.Unlock()
	if reads != seededSensors+3 || mismatches != 0 {
		t.Errorf("reads=%d mismatches=%d: the callback must see the state the change describes", reads, mismatches)
	}
}

func TestOnChange_aPanickingCallbackIsRecoveredCountedAndTheMonitorCarriesOn(t *testing.T) {
	rec := newChangeRecorder()
	var calls int
	r := newRig(t, func(c *Config) {
		c.OnChange = func(ch Change) {
			calls++ // always on the one goroutine that mutates the tracker
			if calls == 1 || calls == 3 {
				panic("boom")
			}
			rec.record(ch)
		}
	}).start()
	st := r.waitReady()
	if st.Tracked != seededSensors {
		t.Fatalf("a panicking callback must not break synchronization: %+v", st)
	}
	if r.m.HookPanics() != 2 {
		t.Errorf("hook panics = %d, want 2", r.m.HookPanics())
	}
	if got := rec.all(); len(got) != seededSensors-2 {
		t.Errorf("the other callbacks still ran: %d recorded", len(got))
	}

	r.srv.Update("sensor.temp", "25.0", tempAttrs)
	r.waitSensor("sensor.temp", "25.0")
	rec.waitLen(t, seededSensors-2+1)
	if !r.m.Status().Healthy {
		t.Error("the monitor must stay healthy")
	}
}

func TestOnChange_unsetIsTheOriginalBehaviourAndConsumesNoSequence(t *testing.T) {
	r := newRig(t, nil).start()
	st := r.waitReady()
	r.srv.Update("sensor.temp", "24.0", tempAttrs)
	r.waitSensor("sensor.temp", "24.0")

	if st.Tracked != seededSensors || r.m.HookPanics() != 0 {
		t.Errorf("status = %+v", st)
	}
	tr := r.m.Tracker()
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	if tr.seq != 0 {
		t.Errorf("with no callback no Change is built and no sequence number is used: seq=%d", tr.seq)
	}
}
