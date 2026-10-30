package monitor

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func sortedSensors(tr *Tracker) []Sensor {
	all := tr.All()
	slices.SortFunc(all, func(a, b Sensor) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	return all
}

func TestChange_firstObservationIsABaseline_thenTransitionsCarryPrevAndCur(t *testing.T) {
	tr := NewTracker()
	t1, t2 := base, base.Add(time.Second)

	res, first, ok := tr.applyChange(SourceLive, upd("sensor.a", "20", 1, 1), t1)
	if !ok || res.Outcome != OutcomeApplied {
		t.Fatalf("first update: %+v ok=%v", res, ok)
	}
	if first.HadPrev || first.Prev != (Sensor{}) || first.Removed {
		t.Errorf("a first observation must be a baseline with no previous state: %+v", first)
	}
	if first.Cur.Raw != "20" || first.Cur.Revision != 1 || first.Seq != 1 || first.Source != SourceLive || first.ID != "sensor.a" {
		t.Errorf("first change = %+v", first)
	}
	if !first.At.Equal(t1) || !first.Cur.ObservedAt.Equal(t1) {
		t.Errorf("At and Cur.ObservedAt must both be the application time: %+v", first)
	}

	_, second, ok := tr.applyChange(SourceReplay, upd("sensor.a", "21", 2, 2), t2)
	if !ok {
		t.Fatal("a newer update must produce a change")
	}
	if !second.HadPrev || second.Prev.Raw != "20" || second.Prev.Revision != 1 || second.Cur.Raw != "21" || second.Cur.Revision != 2 {
		t.Errorf("a transition must carry the previous and the current state: %+v", second)
	}
	if second.Seq != 2 || second.Source != SourceReplay || !second.At.Equal(t2) {
		t.Errorf("second change = %+v", second)
	}
}

func TestChange_attributeOnlyUpdateIsAChangeWhoseValueDidNotMove(t *testing.T) {
	tr := NewTracker()
	tr.applyChange(SourceLive, upd("sensor.a", "20", 1, 1), base)
	moved := upd("sensor.a", "20", 1, 2)
	moved.Unit = "other"
	res, ch, ok := tr.applyChange(SourceLive, moved, base)
	if !ok || res.Outcome != OutcomeApplied || res.ValueChanged {
		t.Fatalf("res = %+v ok=%v", res, ok)
	}
	if !ch.HadPrev || ch.Prev.Raw != ch.Cur.Raw || ch.Prev.Unit == ch.Cur.Unit {
		t.Errorf("the consumer must be able to see that only an attribute moved: %+v", ch)
	}
}

func TestChange_duplicateStaleDroppedAndUntrackedProduceNoChangeAndConsumeNoSequence(t *testing.T) {
	tr := NewTracker()
	_, first, _ := tr.applyChange(SourceLive, upd("sensor.a", "20", 5, 5), base)

	silent := map[string]Update{
		"duplicate":       upd("sensor.a", "20", 5, 5),
		"stale":           upd("sensor.a", "19", 1, 1),
		"untracked":       {ID: "sensor.never", Category: ""},
		"unknown removal": {ID: "sensor.never", Removed: true},
		"stale removal":   {ID: "sensor.a", Removed: true, PrevUpdated: base.Add(time.Millisecond)},
	}
	want := map[string]Outcome{"duplicate": OutcomeDuplicate, "stale": OutcomeStale, "untracked": OutcomeUntracked, "unknown removal": OutcomeUntracked, "stale removal": OutcomeStale}
	for name, u := range silent {
		res, ch, ok := tr.applyChange(SourceLive, u, base)
		if ok || ch != (Change{}) || res.Outcome != want[name] {
			t.Errorf("%s: res=%+v ok=%v change=%+v", name, res, ok, ch)
		}
	}
	if _, still := tr.Get("sensor.a"); !still {
		t.Error("a stale removal must not remove the sensor")
	}

	full := NewTracker()
	for i := 0; i < MaxTracked; i++ {
		full.Apply(upd(fmt.Sprintf("sensor.f%05d", i), "1", 1, 1), base)
	}
	res, ch, ok := full.applyChange(SourceLive, upd("sensor.overflow", "1", 1, 1), base)
	if ok || ch != (Change{}) || res.Outcome != OutcomeDropped {
		t.Errorf("a dropped update must not look like a change: %+v %v %+v", res, ok, ch)
	}

	_, next, ok := tr.applyChange(SourceLive, upd("sensor.a", "22", 9, 9), base)
	if !ok || next.Seq != first.Seq+1 {
		t.Errorf("silent outcomes must not consume sequence numbers: first=%d next=%d", first.Seq, next.Seq)
	}
}

func TestChange_removalAndReAdditionEstablishAFreshBaseline(t *testing.T) {
	tr := NewTracker()
	tr.applyChange(SourceLive, upd("sensor.a", "20", 1, 1), base)
	tr.applyChange(SourceLive, upd("sensor.a", "21", 2, 2), base)

	removal := Update{ID: "sensor.a", Removed: true, PrevUpdated: base.Add(2 * time.Millisecond)}
	res, gone, ok := tr.applyChange(SourceLive, removal, base.Add(time.Second))
	if !ok || res.Outcome != OutcomeRemoved {
		t.Fatalf("removal: %+v ok=%v", res, ok)
	}
	if !gone.Removed || !gone.HadPrev || gone.Prev.Raw != "21" || gone.Cur != (Sensor{}) {
		t.Errorf("a removal reports the last state and no current state: %+v", gone)
	}
	if _, still := tr.Get("sensor.a"); still {
		t.Fatal("the sensor must be gone")
	}

	_, again, ok := tr.applyChange(SourceLive, upd("sensor.a", "30", 10, 10), base.Add(2*time.Second))
	if !ok || again.HadPrev || again.Prev != (Sensor{}) || again.Cur.Revision != 1 {
		t.Errorf("a sensor added again is a first observation, not a transition: %+v", again)
	}
	if gone.Seq >= again.Seq {
		t.Errorf("sequence must keep increasing across removal and re-addition: %d then %d", gone.Seq, again.Seq)
	}

	// A sensor that stops being classified is removed the same way.
	_, lost, ok := tr.applyChange(SourceLive, Update{ID: "sensor.a", Category: "", LastUpdated: base.Add(time.Hour)}, base.Add(3*time.Second))
	if !ok || !lost.Removed || lost.Prev.Raw != "30" {
		t.Errorf("losing the classification is a removal: %+v", lost)
	}
}

func TestChange_sequenceIsStrictlyIncreasingAcrossApplyAndReconcile(t *testing.T) {
	tr := NewTracker()
	var seqs []uint64
	collect := func(c ...Change) {
		for _, ch := range c {
			seqs = append(seqs, ch.Seq)
		}
	}
	for i := 0; i < 5; i++ {
		_, ch, _ := tr.applyChange(SourceLive, upd(fmt.Sprintf("sensor.s%d", i), "1", 1, 1), base)
		collect(ch)
	}
	_, ch, _ := tr.applyChange(SourceLive, upd("sensor.s1", "2", 2, 2), base)
	collect(ch)
	_, changes := tr.reconcileChanges([]Update{upd("sensor.s0", "9", 9, 9), upd("sensor.s7", "1", 1, 1)}, base)
	collect(changes...)
	_, ch, _ = tr.applyChange(SourceLive, upd("sensor.s7", "3", 3, 3), base)
	collect(ch)
	for i, s := range seqs {
		if s != uint64(i+1) {
			t.Fatalf("sequence must be 1,2,3,... without gaps or repeats, got %v", seqs)
		}
	}
	if len(seqs) < 9 {
		t.Fatalf("expected at least 9 changes, got %v", seqs)
	}
}

func TestReconcileChanges_isCompleteDeterministicAndOrdered(t *testing.T) {
	tr := NewTracker()
	for _, id := range []string{"sensor.keep", "sensor.change", "sensor.r2", "sensor.r1"} {
		tr.Apply(upd(id, "1", 1, 1), base)
	}
	now := base.Add(time.Minute)
	snapshot := []Update{
		upd("sensor.keep", "1", 1, 1),   // identical: unchanged, no change
		upd("sensor.change", "2", 2, 2), // updated
		upd("sensor.new2", "5", 5, 5),   // added
		upd("sensor.new1", "6", 6, 6),   // added
		upd("sensor.new1", "7", 7, 7),   // duplicate id in the snapshot: ignored
	}
	sum, changes := tr.reconcileChanges(snapshot, now)

	if sum != (Reconciled{Added: 2, Updated: 1, Unchanged: 1, Removed: 2}) {
		t.Fatalf("summary = %+v", sum)
	}
	var order []string
	for _, c := range changes {
		order = append(order, string(c.ID))
		if c.Source != SourceSnapshot || !c.At.Equal(now) {
			t.Errorf("every snapshot change has the snapshot source and time: %+v", c)
		}
	}
	wantOrder := []string{"sensor.change", "sensor.new2", "sensor.new1", "sensor.r1", "sensor.r2"}
	if !slices.Equal(order, wantOrder) {
		t.Fatalf("order = %v, want %v (snapshot order, then removals sorted by ID)", order, wantOrder)
	}
	if c := changes[0]; !c.HadPrev || c.Prev.Raw != "1" || c.Cur.Raw != "2" {
		t.Errorf("update: %+v", c)
	}
	if c := changes[1]; c.HadPrev || c.Cur.Raw != "5" {
		t.Errorf("an added sensor is a baseline: %+v", c)
	}
	if c := changes[3]; !c.Removed || !c.HadPrev || c.Prev.Raw != "1" || c.Cur != (Sensor{}) {
		t.Errorf("removal: %+v", c)
	}
	for i := 1; i < len(changes); i++ {
		if changes[i].Seq != changes[i-1].Seq+1 {
			t.Errorf("sequence inside one reconcile must be consecutive: %v", changes)
		}
	}
}

func TestReconcileChanges_aRepeatedSnapshotChangesNothing(t *testing.T) {
	tr := NewTracker()
	snap := []Update{upd("sensor.a", "1", 1, 1), upd("binary_sensor.b", "off", 1, 1)}
	_, first := tr.reconcileChanges(snap, base)
	_, second := tr.reconcileChanges(snap, base.Add(time.Minute))
	if len(first) != 2 || first[0].HadPrev || first[1].HadPrev {
		t.Errorf("the first snapshot is all baseline: %+v", first)
	}
	if len(second) != 0 {
		t.Errorf("an identical snapshot must produce no changes: %+v", second)
	}
}

// The change feed is observation only: the same inputs through Apply/Reconcile
// and through applyChange/reconcileChanges leave identical state and counters.
func TestChangeFeed_doesNotAlterTrackerBehaviourOrCounters(t *testing.T) {
	type step struct {
		name      string
		apply     *Update
		reconcile []Update
	}
	u := func(x Update) *Update { return &x }
	attr := upd("sensor.a", "20", 1, 3)
	attr.Unit = "x"
	steps := []step{
		{name: "add", apply: u(upd("sensor.a", "20", 1, 1))},
		{name: "duplicate", apply: u(upd("sensor.a", "20", 1, 1))},
		{name: "stale", apply: u(upd("sensor.a", "10", 0, 0))},
		{name: "newer value", apply: u(upd("sensor.a", "21", 2, 2))},
		{name: "attribute only", apply: u(attr)},
		{name: "second sensor", apply: u(upd("binary_sensor.b", "off", 1, 1))},
		{name: "removal", apply: u(Update{ID: "binary_sensor.b", Removed: true, PrevUpdated: base.Add(time.Millisecond)})},
		{name: "unknown removal", apply: u(Update{ID: "sensor.zzz", Removed: true})},
		{name: "class lost", apply: u(Update{ID: "sensor.a", Category: "", LastUpdated: base.Add(time.Hour)})},
		{name: "re-add", apply: u(upd("sensor.a", "5", 9, 9))},
		{name: "reconcile", reconcile: []Update{upd("sensor.a", "6", 10, 10), upd("sensor.n", "1", 1, 1), upd("sensor.n", "2", 2, 2)}},
		{name: "reconcile again", reconcile: []Update{upd("sensor.a", "6", 10, 10)}},
	}
	plain, feed := NewTracker(), NewTracker()
	now := base.Add(time.Hour)
	for _, s := range steps {
		var a, b string
		if s.apply != nil {
			ra := plain.Apply(*s.apply, now)
			rb, _, _ := feed.applyChange(SourceLive, *s.apply, now)
			a, b = fmt.Sprintf("%+v", ra), fmt.Sprintf("%+v", rb)
		} else {
			ra := plain.Reconcile(s.reconcile, now)
			rb, _ := feed.reconcileChanges(s.reconcile, now)
			a, b = fmt.Sprintf("%+v", ra), fmt.Sprintf("%+v", rb)
		}
		if a != b {
			t.Errorf("%s: results differ: %s vs %s", s.name, a, b)
		}
		if plain.Counters() != feed.Counters() {
			t.Errorf("%s: counters differ: %+v vs %+v", s.name, plain.Counters(), feed.Counters())
		}
		if !slices.Equal(sortedSensors(plain), sortedSensors(feed)) {
			t.Errorf("%s: stored state differs", s.name)
		}
	}
	if plain.seq != 0 {
		t.Errorf("the plain API must not consume sequence numbers: %d", plain.seq)
	}
}
