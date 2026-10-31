package notify

import (
	"fmt"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/monitor"
)

func TestEnters_everyCategoryTriggerAndUnreliableTransition(t *testing.T) {
	live := monitor.SourceLive
	cases := []struct {
		name      string
		r         cachedRule
		cat       monitor.Category
		hadPrev   bool
		prev, cur string
		want      bool
		fromUnrel bool
	}{
		{"door closed to open", rule(1, KindState, 0), monitor.CategoryDoor, true, "off", "on", true, false},
		{"window closed to open", rule(1, KindState, 0), monitor.CategoryWindow, true, "off", "on", true, false},
		{"motion clear to detected", rule(1, KindState, 0), monitor.CategoryMotion, true, "off", "on", true, false},
		{"leak dry to wet", rule(1, KindState, 0), monitor.CategoryLeak, true, "off", "on", true, false},
		{"battery low normal to low", rule(1, KindState, 0), monitor.CategoryBatteryLow, true, "off", "on", true, false},
		{"temperature rises above", rule(1, KindAbove, 28), monitor.CategoryTemperature, true, "20", "29", true, false},
		{"battery falls below", rule(1, KindBelow, 15), monitor.CategoryBattery, true, "20", "10", true, false},
		{"humidity rises above", rule(1, KindAbove, 60), monitor.CategoryHumidity, true, "50", "70", true, false},
		{"already violating is not a transition", rule(1, KindAbove, 28), monitor.CategoryTemperature, true, "29", "30", false, false},
		{"threshold itself is not above", rule(1, KindAbove, 28), monitor.CategoryTemperature, true, "20", "28", false, false},
		{"threshold itself is not below", rule(1, KindBelow, 15), monitor.CategoryBattery, true, "20", "15", false, false},
		{"leaving the state is not a transition", rule(1, KindState, 0), monitor.CategoryDoor, true, "on", "off", false, false},
		{"first observation never qualifies", rule(1, KindState, 0), monitor.CategoryDoor, false, "", "on", false, false},
		{"door unavailable to open is silent", rule(1, KindState, 0), monitor.CategoryDoor, true, "unavailable", "on", false, false},
		{"motion unknown to detected is silent", rule(1, KindState, 0), monitor.CategoryMotion, true, "unknown", "on", false, false},
		{"unparseable to open is silent", rule(1, KindState, 0), monitor.CategoryDoor, true, "garbage", "on", false, false},
		{"temperature unavailable to above is silent", rule(1, KindAbove, 28), monitor.CategoryTemperature, true, "unavailable", "29", false, false},
		{"leak unavailable to wet fails loud", rule(1, KindState, 0), monitor.CategoryLeak, true, "unavailable", "on", true, true},
		{"leak unknown to wet fails loud", rule(1, KindState, 0), monitor.CategoryLeak, true, "unknown", "on", true, true},
		{"open to unavailable is not a violation", rule(1, KindState, 0), monitor.CategoryDoor, true, "off", "unavailable", false, false},
		{"state rule on a numeric sensor never fires", rule(1, KindState, 0), monitor.CategoryTemperature, true, "20", "29", false, false},
		{"above rule on a binary sensor never fires", rule(1, KindAbove, 1), monitor.CategoryDoor, true, "off", "on", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.r.Kind != KindState {
				tc.r.Unit = unitFor(tc.cat) // a numeric rule is bound to the unit its sensor reports
			}
			got, unrel := enters(tc.r, chg("x.sensor", tc.cat, tc.hadPrev, tc.prev, tc.cur, live, 1))
			if got != tc.want || unrel != tc.fromUnrel {
				t.Errorf("enters = %v (fromUnreliable %v), want %v (%v)", got, unrel, tc.want, tc.fromUnrel)
			}
		})
	}
	removal := chg("x.sensor", monitor.CategoryDoor, true, "off", "on", live, 1)
	removal.Removed = true
	if q, _ := enters(rule(1, KindState, 0), removal); q {
		t.Error("a removal never qualifies")
	}
}

func TestCooldown_categoryDefaultsAndOverrides(t *testing.T) {
	defaults := map[monitor.Category]time.Duration{
		monitor.CategoryMotion: 5 * time.Minute, monitor.CategoryDoor: time.Minute, monitor.CategoryWindow: time.Minute,
		monitor.CategoryLeak: 15 * time.Minute, monitor.CategoryTemperature: 15 * time.Minute, monitor.CategoryHumidity: 15 * time.Minute,
		monitor.CategoryBattery: 6 * time.Hour, monitor.CategoryBatteryLow: 6 * time.Hour,
	}
	for cat, want := range defaults {
		if got := cooldownOf(cachedRule{}, cat); got != want {
			t.Errorf("%s default = %v, want %v", cat, got, want)
		}
	}
	override, zero := 90*time.Second, time.Duration(0)
	if got := cooldownOf(cachedRule{Cooldown: &override}, monitor.CategoryBattery); got != override {
		t.Errorf("an override takes precedence: %v", got)
	}
	if got := cooldownOf(cachedRule{Cooldown: &zero}, monitor.CategoryBattery); got != 0 {
		t.Errorf("an explicit zero disables the cooldown: %v", got)
	}
}

func rulesFor(ids ...int64) []cachedRule {
	out := make([]cachedRule, len(ids))
	for i, id := range ids {
		out[i] = rule(id, KindState, 0)
	}
	return out
}

func TestMailbox_firstObservationAndRemovalNeverLatch_andRequestAReset(t *testing.T) {
	mb := newMailbox(MaxRules)
	rs := rulesFor(1)
	mb.record(chg("x.sensor", monitor.CategoryDoor, false, "", "on", monitor.SourceSnapshot, 1), rs)
	slots, order, _ := mb.drain()
	s := slots["x.sensor"]
	if len(order) != 1 || len(s.latches) != 0 || !s.reset || !s.anySnapshot || s.cur.Raw != "on" {
		t.Fatalf("a first observation is a baseline: %+v", s)
	}

	mb.record(chg("x.sensor", monitor.CategoryDoor, true, "off", "on", monitor.SourceLive, 2), rs)
	rm := chg("x.sensor", monitor.CategoryDoor, true, "on", "on", monitor.SourceLive, 3)
	rm.Removed = true
	mb.record(rm, rs)
	slots, _, _ = mb.drain()
	s = slots["x.sensor"]
	if !s.removed || !s.reset || s.haveCur || len(s.latches) != 0 {
		t.Fatalf("a removal discards the pending latch and asks every rule to re-baseline: %+v", s)
	}

	mb.record(rm, rs) // removed again, then re-added as a first observation
	mb.record(chg("x.sensor", monitor.CategoryDoor, false, "", "off", monitor.SourceLive, 4), rs)
	slots, _, _ = mb.drain()
	if s = slots["x.sensor"]; s.removed || !s.haveCur || !s.reset || len(s.latches) != 0 {
		t.Errorf("re-addition is a fresh baseline: %+v", s)
	}
}

func TestMailbox_aViolationThatEndsBeforeEvaluationStillLatches(t *testing.T) {
	mb := newMailbox(MaxRules)
	rs := rulesFor(7)
	mb.record(chg("x.sensor", monitor.CategoryDoor, true, "off", "on", monitor.SourceLive, 10), rs)
	mb.record(chg("x.sensor", monitor.CategoryDoor, true, "on", "off", monitor.SourceLive, 11), rs)
	slots, _, _ := mb.drain()
	s := slots["x.sensor"]
	l, ok := s.latchFor(7)
	if !ok || l.seq != 10 || l.source != monitor.SourceLive || l.sensor.Raw != "on" || l.fromUnreliable {
		t.Fatalf("the latch must keep the qualifying transition: %+v ok=%v", l, ok)
	}
	if s.cur.Raw != "off" || s.lastSeq != 11 {
		t.Errorf("the slot also keeps the latest state: %+v", s)
	}
}

func TestMailbox_keepsTheFirstQualifyingTransition_andPreservesItsSource(t *testing.T) {
	mb := newMailbox(MaxRules)
	rs := rulesFor(1)
	mb.record(chg("x.sensor", monitor.CategoryDoor, true, "off", "on", monitor.SourceSnapshot, 5), rs)
	mb.record(chg("x.sensor", monitor.CategoryDoor, true, "on", "off", monitor.SourceReplay, 6), rs)
	mb.record(chg("x.sensor", monitor.CategoryDoor, true, "off", "on", monitor.SourceLive, 7), rs)
	slots, _, _ := mb.drain()
	first := slots["x.sensor"]
	l, _ := first.latchFor(1)
	if l.seq != 5 || l.source != monitor.SourceSnapshot {
		t.Errorf("the FIRST qualifying transition and its source survive coalescing: %+v", l)
	}
	if mb.coalesced.Load() != 2 {
		t.Errorf("coalesced = %d, want 2", mb.coalesced.Load())
	}
}

func TestMailbox_isBoundedByWatchedEntities_andKeepsFirstDirtyOrder(t *testing.T) {
	mb := newMailbox(MaxRules)
	for i := 0; i < 5000; i++ {
		id := fmt.Sprintf("binary_sensor.e%d", i%3)
		mb.record(chg(id, monitor.CategoryDoor, true, "off", "on", monitor.SourceLive, uint64(i+1)), rulesFor(1))
	}
	slots, order, over := mb.drain()
	if len(slots) != 3 || len(order) != 3 || over {
		t.Fatalf("5000 events over 3 entities must occupy 3 slots: %d %d %v", len(slots), len(order), over)
	}
	if order[0] != "binary_sensor.e0" || order[1] != "binary_sensor.e1" || order[2] != "binary_sensor.e2" {
		t.Errorf("entities are drained in the order they first became dirty: %v", order)
	}
	if p, _ := mb.pending(time.Unix(1_000_200, 0)); p != 0 {
		t.Error("a drained mailbox is empty")
	}

	small := newMailbox(2)
	for i := 0; i < 4; i++ {
		small.record(chg(fmt.Sprintf("binary_sensor.e%d", i), monitor.CategoryDoor, true, "off", "on", monitor.SourceLive, uint64(i+1)), rulesFor(1))
	}
	slots, _, over = small.drain()
	if len(slots) != 2 || !over || small.overflows.Load() != 2 {
		t.Errorf("beyond its cap the mailbox refuses and flags a full resync: %d slots, overflowed=%v", len(slots), over)
	}
}

func TestQueue_normalLaneDropsOldest_andReportsEveryDrop(t *testing.T) {
	q := newQueue(2, 3)
	now := time.Unix(1_000_000, 0)
	mk := func(rule int64) item {
		return item{msg: Message{Kind: MsgAlert, RuleID: rule, Category: monitor.CategoryDoor}, deadline: now.Add(time.Hour)}
	}
	for i := int64(1); i <= 3; i++ {
		if d := q.push(mk(i), false); len(d) != 0 {
			t.Fatalf("no drop while there is room: %v", d)
		}
	}
	d := q.push(mk(4), false)
	if len(d) != 1 || d[0].it.msg.RuleID != 1 || d[0].reason != dropOverflow {
		t.Fatalf("the oldest normal message is dropped and reported: %+v", d)
	}
	var order []int64
	for {
		it, ok, _ := q.pop(now)
		if !ok {
			break
		}
		order = append(order, it.msg.RuleID)
	}
	if fmt.Sprint(order) != "[2 3 4]" {
		t.Errorf("FIFO order is kept: %v", order)
	}
}

func TestQueue_theCriticalLaneNeverEvictsAnActiveLeakForALessImportantMessage(t *testing.T) {
	q := newQueue(2, 4)
	now := time.Unix(1_000_000, 0)
	leak := func(rule int64) item {
		return item{msg: Message{Kind: MsgAlert, RuleID: rule, Category: monitor.CategoryLeak}, deadline: now.Add(time.Hour)}
	}
	other := func(k MessageKind) item { return item{msg: Message{Kind: k}, deadline: now.Add(time.Hour)} }
	q.push(leak(1), false)
	q.push(leak(2), false)
	for _, k := range []MessageKind{MsgDigest, MsgMonitoringRecovered, MsgMonitoringDown} {
		d := q.push(other(k), false)
		if len(d) != 1 || d[0].it.msg.Kind != k || d[0].reason != dropOverflow {
			t.Errorf("a %d message must be the one rejected, never a queued leak alert: %+v", k, d)
		}
	}
	d := q.push(leak(3), false)
	if len(d) != 1 || d[0].it.msg.RuleID != 1 {
		t.Errorf("only an equally important newcomer evicts, and it evicts the OLDEST leak, which is reported: %+v", d)
	}

	q = newQueue(3, 4)
	q.push(other(MsgMonitoringRecovered), false)
	q.push(other(MsgDigest), false)
	q.push(other(MsgMonitoringDown), false)
	d = q.push(leak(9), false)
	if len(d) != 1 || d[0].it.msg.Kind != MsgMonitoringRecovered {
		t.Errorf("the lowest-priority message (a recovery notice) is evicted first: %+v", d)
	}
}

func TestQueue_popsCriticalFirst_andRespectsNotBefore(t *testing.T) {
	q := newQueue(4, 4)
	now := time.Unix(1_000_000, 0)
	norm := item{msg: Message{Kind: MsgAlert, RuleID: 1, Category: monitor.CategoryDoor}}
	crit := item{msg: Message{Kind: MsgAlert, RuleID: 2, Category: monitor.CategoryLeak}}
	waiting := item{msg: Message{Kind: MsgAlert, RuleID: 3, Category: monitor.CategoryLeak}, notBefore: now.Add(time.Minute)}
	q.push(norm, false)
	q.push(waiting, false)
	q.push(crit, false)
	if it, ok, _ := q.pop(now); !ok || it.msg.RuleID != 2 {
		t.Fatalf("an eligible critical message goes first: %+v", it)
	}
	if it, ok, _ := q.pop(now); !ok || it.msg.RuleID != 1 {
		t.Fatalf("then the normal one, because the other critical one is still waiting to retry: %+v", it)
	}
	if _, ok, next := q.pop(now); ok || !next.Equal(now.Add(time.Minute)) {
		t.Fatalf("nothing is eligible and the next retry time is reported: ok=%v next=%v", ok, next)
	}
	if it, ok, _ := q.pop(now.Add(time.Minute)); !ok || it.msg.RuleID != 3 {
		t.Errorf("the waiting message becomes eligible: %+v", it)
	}
}

func TestBackoffAndTokenBucket(t *testing.T) {
	full := retryBackoff{initial: time.Second, max: 30 * time.Second, rand: func(n int64) int64 { return n - 1 }}
	zero := retryBackoff{initial: time.Second, max: 30 * time.Second, rand: func(int64) int64 { return 0 }}
	for attempt, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second} {
		if got := full.delay(attempt + 1); got != want {
			t.Errorf("full jitter delay(%d) = %v, want %v", attempt+1, got, want)
		}
	}
	if got := zero.delay(3); got != 2*time.Second {
		t.Errorf("equal jitter keeps half the delay fixed: %v", got)
	}

	b := newTokenBucket(1, 5)
	now := time.Unix(1_000_000, 0)
	for i := 0; i < 5; i++ {
		if w := b.wait(now); w != 0 {
			t.Fatalf("the burst of 5 is immediate, token %d waits %v", i+1, w)
		}
	}
	if w := b.wait(now); w != time.Second {
		t.Errorf("the sixth token waits one second: %v", w)
	}
	if w := b.wait(now.Add(10 * time.Second)); w != 0 {
		t.Errorf("tokens refill with time: %v", w)
	}
}
