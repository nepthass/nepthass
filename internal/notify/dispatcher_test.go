package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
)

type recSink struct {
	mu         sync.Mutex
	deliveredI []item
	droppedD   []drop
}

func (s *recSink) delivered(it item) {
	s.mu.Lock()
	s.deliveredI = append(s.deliveredI, it)
	s.mu.Unlock()
}

func (s *recSink) dropped(d drop) {
	s.mu.Lock()
	s.droppedD = append(s.droppedD, d)
	s.mu.Unlock()
}

func (s *recSink) reasons() []dropReason {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []dropReason
	for _, d := range s.droppedD {
		out = append(out, d.reason)
	}
	return out
}

type mapAccess struct {
	mu  sync.Mutex
	m   map[entities.ID]entities.Access
	err error
}

func (a *mapAccess) Get(_ context.Context, id entities.ID) (entities.Access, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return entities.AccessNone, a.err
	}
	return a.m[id], nil
}

func (a *mapAccess) set(id entities.ID, v entities.Access) {
	a.mu.Lock()
	a.m[id] = v
	a.mu.Unlock()
}

type dfix struct {
	clk    *fakeClock
	snd    *fakeSender
	q      *queue
	d      *dispatcher
	acc    *mapAccess
	sink   *recSink
	m      *counters
	active atomic.Bool
}

func newDFix(mod func(*dispatcherConfig)) *dfix {
	f := &dfix{clk: newFakeClock(), snd: newFakeSender(), q: newQueue(32, 64), acc: &mapAccess{m: map[entities.ID]entities.Access{"door.a": entities.AccessRead}},
		sink: &recSink{}, m: &counters{}}
	f.active.Store(true)
	cfg := dispatcherConfig{ttlDrain: 3 * time.Second, rate: 1000, burst: 1000, threshold: 3, probe: time.Minute,
		backoff: retryBackoff{initial: time.Second, max: 30 * time.Second, rand: func(n int64) int64 { return n - 1 }}}
	if mod != nil {
		mod(&cfg)
	}
	f.d = newDispatcher(f.q, f.snd, f.acc, f.clk, cfg, f.active.Load, f.sink, slog.New(slog.NewTextHandler(io.Discard, nil)), f.m)
	return f
}

func (f *dfix) alert(entity string, cat monitor.Category) item {
	now := f.clk.Now()
	return item{msg: Message{Kind: MsgAlert, RuleID: 1, Category: cat, Name: "N", ObservedAt: now, entity: entities.ID(entity)}, deadline: now.Add(10 * time.Minute)}
}

func (f *dfix) process(it item) { f.d.process(ctx, ctx, it, false) }

// pump processes every message that is eligible now, in queue order.
func (f *dfix) pump() {
	for {
		it, ok, _ := f.q.pop(f.clk.Now())
		if !ok {
			return
		}
		f.process(it)
	}
}

func TestDispatcher_deliversAndCountsSuccess(t *testing.T) {
	f := newDFix(nil)
	f.process(f.alert("door.a", monitor.CategoryDoor))
	if f.snd.waitCalls(t, 1)[0].Name != "N" || f.m.delivered.Load() != 1 || len(f.sink.deliveredI) != 1 || len(f.sink.reasons()) != 0 {
		t.Errorf("delivered=%d reasons=%v", f.m.delivered.Load(), f.sink.reasons())
	}
}

func TestDispatcher_retriesRetryableFailuresWithBackoff_untilTheAbsoluteTTL(t *testing.T) {
	f := newDFix(nil)
	f.snd.script = func(int, Message) error { return Retryable(errors.New("network")) }
	first := f.alert("door.a", monitor.CategoryDoor)
	deadline := first.deadline
	f.process(first)

	var delays []time.Duration
	for i := 0; i < 100 && len(f.sink.reasons()) == 0; i++ {
		it, ok, next := f.q.pop(f.clk.Now())
		if !ok {
			delays = append(delays, next.Sub(f.clk.Now()))
			f.clk.Advance(next.Sub(f.clk.Now()))
			continue
		}
		if !it.deadline.Equal(deadline) {
			t.Fatalf("the deadline is absolute: retries must not extend it (%v != %v)", it.deadline, deadline)
		}
		f.process(it)
	}
	if got := f.sink.reasons(); len(got) != 1 || got[0] != dropTTL {
		t.Fatalf("a message that keeps failing is dropped at its deadline: %v", got)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}
	for i, w := range want {
		if delays[i] != w {
			t.Fatalf("backoff delays = %v, want a prefix of %v", delays, want)
		}
	}
	if f.clk.Now().After(deadline.Add(31 * time.Second)) {
		t.Errorf("no attempt may be scheduled past the deadline")
	}
	if f.m.droppedTTL.Load() != 1 || f.m.retried.Load() == 0 {
		t.Errorf("ttl drops=%d retries=%d", f.m.droppedTTL.Load(), f.m.retried.Load())
	}
}

func TestDispatcher_anUnclassifiedErrorIsRetried(t *testing.T) {
	f := newDFix(nil)
	f.snd.script = func(int, Message) error { return errors.New("connection reset") }
	f.process(f.alert("door.a", monitor.CategoryDoor))
	if _, _, next := f.q.pop(f.clk.Now()); next.IsZero() || len(f.sink.reasons()) != 0 {
		t.Errorf("an ambiguous failure must be retried, not dropped: reasons=%v", f.sink.reasons())
	}
}

func TestDispatcher_permanentFailuresAreNotRetried_openTheBreaker_andAProbeRecovers(t *testing.T) {
	f := newDFix(nil)
	f.snd.script = func(int, Message) error { return Permanent(errors.New("missing access")) }
	for i := 0; i < 3; i++ {
		f.process(f.alert("door.a", monitor.CategoryDoor))
	}
	if len(f.snd.messages()) != 3 || !f.d.br.isOpen() {
		t.Fatalf("three permanent failures open the breaker: sends=%d open=%v", len(f.snd.messages()), f.d.br.isOpen())
	}
	if _, _, next := f.q.pop(f.clk.Now()); !next.IsZero() {
		t.Error("a permanent failure is never queued for a retry")
	}
	f.process(f.alert("door.a", monitor.CategoryDoor))
	if len(f.snd.messages()) != 3 || f.m.droppedBreaker.Load() != 1 {
		t.Errorf("while open nothing is sent: sends=%d breaker drops=%d", len(f.snd.messages()), f.m.droppedBreaker.Load())
	}

	f.clk.Advance(time.Minute)
	f.snd.script = func(int, Message) error { return Permanent(errors.New("still broken")) }
	f.process(f.alert("door.a", monitor.CategoryDoor))
	f.process(f.alert("door.a", monitor.CategoryDoor))
	if len(f.snd.messages()) != 4 || !f.d.br.isOpen() {
		t.Errorf("after the interval exactly ONE probe goes out, and a failed probe keeps it open: sends=%d", len(f.snd.messages()))
	}

	f.clk.Advance(time.Minute)
	f.snd.script = nil
	f.process(f.alert("door.a", monitor.CategoryDoor))
	if f.d.br.isOpen() || f.m.delivered.Load() != 1 {
		t.Errorf("a successful probe closes the breaker: open=%v delivered=%d", f.d.br.isOpen(), f.m.delivered.Load())
	}
	f.process(f.alert("door.a", monitor.CategoryDoor))
	if f.m.delivered.Load() != 2 {
		t.Error("delivery resumes normally")
	}
}

func TestDispatcher_aRateLimitIsWaitedOutWithoutCountingAsAFailure(t *testing.T) {
	f := newDFix(nil)
	f.snd.script = func(n int, _ Message) error {
		if n == 1 {
			return RateLimited(errors.New("429"), 5*time.Second)
		}
		return nil
	}
	start := f.clk.Now()
	f.process(f.alert("door.a", monitor.CategoryDoor))
	it, ok, next := f.q.pop(f.clk.Now())
	if ok || !next.Equal(start.Add(5*time.Second)) || !f.d.pauseUntil.Equal(start.Add(5*time.Second)) {
		t.Fatalf("the message waits RetryAfter and the dispatcher pauses: ok=%v next=%v", ok, next)
	}
	f.clk.Advance(5 * time.Second)
	it, ok, _ = f.q.pop(f.clk.Now())
	if !ok || it.attempts != 0 || f.m.retried.Load() != 0 {
		t.Fatalf("a rate limit is not an attempt or a failure: attempts=%d retried=%d", it.attempts, f.m.retried.Load())
	}
	f.process(it)
	if f.m.delivered.Load() != 1 {
		t.Error("delivered after the wait")
	}

	g := newDFix(nil)
	g.snd.script = func(int, Message) error { return RateLimited(errors.New("429"), 20*time.Minute) }
	g.process(g.alert("door.a", monitor.CategoryDoor))
	if got := g.sink.reasons(); len(got) != 1 || got[0] != dropTTL {
		t.Errorf("a wait beyond the deadline is a TTL drop: %v", got)
	}
}

func TestDispatcher_theTTLIsCheckedBeforeEveryAttempt(t *testing.T) {
	f := newDFix(nil)
	it := f.alert("door.a", monitor.CategoryDoor)
	f.clk.Advance(11 * time.Minute)
	f.process(it)
	if len(f.snd.messages()) != 0 || len(f.sink.reasons()) != 1 || f.sink.reasons()[0] != dropTTL {
		t.Errorf("an expired message is never sent: sends=%d reasons=%v", len(f.snd.messages()), f.sink.reasons())
	}
}

func TestDispatcher_sharingIsRecheckedBeforeEverySend_queuedAndDuringRetries(t *testing.T) {
	f := newDFix(nil)
	it := f.alert("door.a", monitor.CategoryDoor)
	f.acc.set("door.a", entities.AccessNone) // revoked while the message waited in the queue
	f.process(it)
	if len(f.snd.messages()) != 0 || f.sink.reasons()[0] != dropRevoked || f.m.droppedRevoked.Load() != 1 {
		t.Fatalf("a revoked entity must never be sent: sends=%d reasons=%v", len(f.snd.messages()), f.sink.reasons())
	}

	g := newDFix(nil)
	g.snd.script = func(int, Message) error { return Retryable(errors.New("network")) }
	g.process(g.alert("door.a", monitor.CategoryDoor))
	g.acc.set("door.a", entities.AccessNone) // revoked between the first attempt and the retry
	_, _, next := g.q.pop(g.clk.Now())
	g.clk.Advance(next.Sub(g.clk.Now()))
	g.pump()
	if len(g.snd.messages()) != 1 || len(g.sink.reasons()) != 1 || g.sink.reasons()[0] != dropRevoked {
		t.Errorf("the retry must re-check: sends=%d reasons=%v", len(g.snd.messages()), g.sink.reasons())
	}
}

func TestDispatcher_digestLinesAreRecheckedIndividually(t *testing.T) {
	f := newDFix(nil)
	f.acc.set("door.b", entities.AccessNone)
	now := f.clk.Now()
	msg := Message{Kind: MsgDigest, ObservedAt: now, Lines: []DigestLine{{Name: "A", entity: "door.a"}, {Name: "B", entity: "door.b"}}}
	f.process(item{msg: msg, deadline: now.Add(time.Hour)})
	sent := f.snd.messages()
	if len(sent) != 1 || len(sent[0].Lines) != 1 || sent[0].Lines[0].Name != "A" {
		t.Fatalf("only the still-shared line may be sent: %+v", sent)
	}
	f.acc.set("door.a", entities.AccessNone)
	f.process(item{msg: msg, deadline: now.Add(time.Hour)})
	if len(f.snd.messages()) != 1 || f.sink.reasons()[0] != dropRevoked {
		t.Errorf("a digest with nothing shared left is not sent: %v", f.sink.reasons())
	}
}

func TestDispatcher_failsClosedWhenSharingCannotBeRead(t *testing.T) {
	f := newDFix(nil)
	f.acc.mu.Lock()
	f.acc.err = errors.New("database is locked")
	f.acc.mu.Unlock()
	f.process(f.alert("door.a", monitor.CategoryDoor))
	if len(f.snd.messages()) != 0 || len(f.sink.reasons()) != 0 {
		t.Fatalf("nothing is sent without a permission answer, and the message is kept: sends=%d reasons=%v", len(f.snd.messages()), f.sink.reasons())
	}
	f.acc.mu.Lock()
	f.acc.err = nil
	f.acc.mu.Unlock()
	_, _, next := f.q.pop(f.clk.Now())
	f.clk.Advance(next.Sub(f.clk.Now()))
	f.pump()
	if f.m.delivered.Load() != 1 {
		t.Error("it is sent once permission can be read")
	}
}

func TestDispatcher_disabledNotificationsDropQueuedMessages(t *testing.T) {
	f := newDFix(nil)
	f.active.Store(false)
	f.process(f.alert("door.a", monitor.CategoryDoor))
	if len(f.snd.messages()) != 0 || f.sink.reasons()[0] != dropDisabled {
		t.Errorf("reasons=%v", f.sink.reasons())
	}
}

func TestDispatcher_theTokenBucketSpacesSends(t *testing.T) {
	f := newDFix(func(c *dispatcherConfig) { c.rate, c.burst = 1, 1 })
	f.process(f.alert("door.a", monitor.CategoryDoor))
	done := make(chan struct{})
	go func() {
		f.process(f.alert("door.a", monitor.CategoryDoor))
		close(done)
	}()
	f.clk.AwaitWaiters(t, 1) // the second send is waiting for a token
	if len(f.snd.messages()) != 1 {
		t.Fatal("the second send must wait for its token")
	}
	f.clk.Advance(time.Second)
	<-done
	if len(f.snd.messages()) != 2 {
		t.Errorf("sends = %d", len(f.snd.messages()))
	}
}

// goroutinesIn lists the engine goroutines still alive.
func goroutinesIn(markers ...string) []string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var found []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		for _, m := range markers {
			if strings.Contains(g, m) {
				found = append(found, m)
			}
		}
	}
	return found
}

func awaitNoGoroutines(t *testing.T, markers ...string) {
	t.Helper()
	deadline := time.After(testWait)
	for {
		if len(goroutinesIn(markers...)) == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("goroutines still running: %v", goroutinesIn(markers...))
		case <-time.After(time.Millisecond): // poll without burning the CPU quota
		}
	}
}

var engineGoroutines = []string{"notify.(*dispatcher).run", "notify.(*Engine).runEvaluator", "notify.(*Engine).runWatcher", "notify.(*dispatcher).callSend.func1"}

func TestDispatcher_shutdownDrainsQueuedMessagesWithinTheBudget_andStopsWaiting(t *testing.T) {
	f := newDFix(nil)
	runCtx, cancel := context.WithCancel(context.Background())
	now := f.clk.Now()
	for i := 0; i < 3; i++ {
		it := f.alert("door.a", monitor.CategoryDoor)
		it.notBefore = now.Add(time.Hour) // waiting to retry: only the drain will send them
		f.q.push(it, false)
	}
	done := make(chan struct{})
	go func() { f.d.run(runCtx); close(done) }()
	f.clk.AwaitWaiters(t, 1) // idle, waiting for the next retry time
	cancel()
	<-done
	if len(f.snd.messages()) != 3 {
		t.Errorf("shutdown must make one attempt per queued message: sends=%d", len(f.snd.messages()))
	}
	awaitNoGoroutines(t, engineGoroutines...)
}

func TestDispatcher_shutdownIsBoundedEvenWhenASendIgnoresCancellation(t *testing.T) {
	f := newDFix(nil)
	gate := make(chan struct{})
	f.snd.gate = gate // Send blocks and ignores ctx
	runCtx, cancel := context.WithCancel(context.Background())
	for i := 0; i < 3; i++ {
		f.q.push(f.alert("door.a", monitor.CategoryDoor), false)
	}
	done := make(chan struct{})
	go func() { f.d.run(runCtx); close(done) }()
	f.snd.waitCalls(t, 1) // the first send is in flight and stuck
	cancel()
	f.clk.AwaitWaiters(t, 1) // the drain budget timer
	f.clk.Advance(3 * time.Second)
	<-done
	if f.m.abandoned.Load() != 1 {
		t.Errorf("the stuck send must be abandoned and counted: %d", f.m.abandoned.Load())
	}
	if f.m.droppedShutdown.Load() < 3 {
		t.Errorf("the abandoned send and the rest of the queue are dropped as shutdown: %d", f.m.droppedShutdown.Load())
	}
	close(gate) // let the abandoned call finish
	awaitNoGoroutines(t, engineGoroutines...)
}

// advancingClock moves time forward just before a timer is registered, which is
// what a real clock does in the gap between reading Now and calling After.
type advancingClock struct {
	*fakeClock
	once sync.Once
	jump time.Duration
}

func (c *advancingClock) After(d time.Duration) <-chan time.Time {
	c.once.Do(func() { c.fakeClock.Advance(c.jump) })
	return c.fakeClock.After(d)
}

func TestDispatcher_doesNotSleepThroughAWakeUpTimeThatPassedWhileItRegisteredItsTimer(t *testing.T) {
	f := newDFix(nil)
	f.d.clock = &advancingClock{fakeClock: f.clk, jump: 3 * time.Second}
	it := f.alert("door.a", monitor.CategoryDoor)
	it.notBefore = f.clk.Now().Add(time.Second) // eligible after one second
	f.q.push(it, false)
	select { // the push left a wake-up token; only the timer may wake the dispatcher in this test
	case <-f.q.wake:
	default:
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.d.run(runCtx); close(done) }()
	f.snd.waitCalls(t, 1) // without the re-check the dispatcher would sleep until a later Advance that never comes
	cancel()
	f.clk.Advance(time.Hour) // releases the shutdown drain timer
	<-done
}
