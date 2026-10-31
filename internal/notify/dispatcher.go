package notify

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
)

// AccessSource answers what access an entity is shared with. *entities.Permissions
// satisfies it.
type AccessSource interface {
	Get(ctx context.Context, id entities.ID) (entities.Access, error)
}

// outcomeSink is told what became of every message that left the queue.
type outcomeSink interface {
	delivered(it item)
	dropped(d drop)
}

type dispatcherConfig struct {
	ttlDrain  time.Duration
	backoff   retryBackoff
	rate      float64
	burst     int
	threshold int
	probe     time.Duration
}

// dispatcher takes messages from the queue one at a time and hands them to the
// Sender. It is the only place that performs a send, and it runs on its own
// goroutine: the monitor callback never reaches it.
//
// Delivery guarantee: best effort, with bounded staleness and possible
// duplicates. It is not exactly-once, not at-least-once and not strictly
// at-most-once. A message is attempted only until its absolute deadline (the TTL
// runs from the observation and retries do not extend it). Retryable failures
// (network errors, ambiguous timeouts, server errors) and rate limits are
// retried with context-aware backoff; permanent failures are dropped and open
// the circuit breaker. Queued and in-flight messages are lost if the process
// dies; at graceful shutdown the queue is drained for a bounded time and the
// rest is dropped and counted.
//
// Authorization: immediately before EVERY attempt the entity's sharing is read
// again and a message whose entity is no longer shared with at least read access
// is dropped, so a revocation cannot be bypassed by a queued or retrying
// message. One race cannot be closed: a revocation that lands after that check
// but before the transport accepts the request (up to the transport's request
// timeout) does not recall a message already in flight.
type dispatcher struct {
	q      *queue
	sender Sender
	access AccessSource
	clock  Clock
	bucket *tokenBucket
	bo     retryBackoff
	br     *breaker
	cfg    dispatcherConfig
	active func() bool
	sink   outcomeSink
	log    *slog.Logger
	m      *counters

	pauseUntil time.Time
	abandoned  atomic.Int64
}

func newDispatcher(q *queue, s Sender, a AccessSource, c Clock, cfg dispatcherConfig, active func() bool, sink outcomeSink, log *slog.Logger, m *counters) *dispatcher {
	return &dispatcher{q: q, sender: s, access: a, clock: c, bucket: newTokenBucket(cfg.rate, cfg.burst), bo: cfg.backoff,
		br: &breaker{threshold: cfg.threshold, probe: cfg.probe}, cfg: cfg, active: active, sink: sink, log: log, m: m}
}

// run processes the queue until ctx ends, then drains it for a bounded time.
func (d *dispatcher) run(ctx context.Context) {
	sendCtx, stopSending := context.WithCancel(context.Background())
	defer stopSending()
	// The drain budget starts the moment shutdown begins, wherever the loop is
	// (including inside a blocked send), so shutdown is always bounded.
	go func() {
		select {
		case <-ctx.Done():
		case <-sendCtx.Done():
			return
		}
		_ = sleep(sendCtx, d.clock, d.cfg.ttlDrain)
		stopSending()
	}()
	for ctx.Err() == nil {
		now := d.clock.Now()
		var it item
		var ok bool
		var next time.Time
		if now.Before(d.pauseUntil) {
			next = d.pauseUntil
		} else {
			it, ok, next = d.q.pop(now)
		}
		if ok {
			d.process(ctx, sendCtx, it, false)
			continue
		}
		var timer <-chan time.Time
		if !next.IsZero() {
			timer = d.clock.After(next.Sub(now))
			if !d.clock.Now().Before(next) {
				// The clock moved past the wake-up time between reading it and registering
				// the timer, which is relative to the new time: do not sleep through it.
				continue
			}
		}
		select {
		case <-d.q.wake:
		case <-timer:
		case <-ctx.Done():
		}
	}
	// Graceful shutdown: one attempt per queued message within the drain budget.
	for sendCtx.Err() == nil {
		it, ok := d.q.popAny()
		if !ok {
			return
		}
		d.process(sendCtx, sendCtx, it, true)
	}
	for {
		it, ok := d.q.popAny()
		if !ok {
			return
		}
		d.drop(it, dropShutdown)
	}
}

type verdict uint8

const (
	verdictSend verdict = iota
	verdictRevoked
	verdictRetry
)

// process makes one attempt at one message. waitCtx bounds waits (it ends at
// shutdown) and sendCtx bounds the transport call. When draining there are no retries.
func (d *dispatcher) process(waitCtx, sendCtx context.Context, it item, draining bool) {
	now := d.clock.Now()
	switch {
	case !d.active():
		d.drop(it, dropDisabled)
		return
	case now.After(it.deadline):
		d.drop(it, dropTTL)
		return
	}
	it, v := d.recheck(sendCtx, it)
	switch v {
	case verdictRevoked:
		d.drop(it, dropRevoked)
		return
	case verdictRetry:
		d.retryLater(it, now, draining)
		return
	}
	// Asked only now: when the breaker is open and its probe interval has passed,
	// allow takes the single probe slot, so a message that is revoked or whose
	// permission could not be read must not be the one to consume it.
	if !d.br.allow(now) {
		d.drop(it, dropBreaker)
		return
	}
	if w := d.bucket.wait(now); w > 0 {
		if err := sleep(waitCtx, d.clock, w); err != nil {
			d.retryLater(it, now, true) // shutdown began while waiting for a token
			return
		}
		if d.clock.Now().After(it.deadline) {
			d.drop(it, dropTTL)
			return
		}
	}
	err := d.callSend(sendCtx, it.msg)
	switch {
	case err == nil:
		if d.br.success() {
			d.log.Info("notification delivery recovered")
		}
		d.m.delivered.Add(1)
		d.sink.delivered(it)
	case sendCtx.Err() != nil && errors.Is(err, sendCtx.Err()):
		d.drop(it, dropShutdown)
	default:
		kind, after := classify(err)
		switch kind {
		case FailurePermanent:
			d.br.permanentFailure(now)
			d.drop(it, dropPermanent)
		case FailureRateLimited:
			if after <= 0 {
				after = time.Second
			}
			if draining || now.Add(after).After(it.deadline) {
				d.drop(it, dropTTLOrShutdown(draining))
				return
			}
			d.pauseUntil = now.Add(after)
			it.notBefore = d.pauseUntil
			d.requeue(it)
		default:
			d.m.retried.Add(1)
			d.retryLater(it, now, draining)
		}
	}
}

func dropTTLOrShutdown(draining bool) dropReason {
	if draining {
		return dropShutdown
	}
	return dropTTL
}

// retryLater schedules the next attempt with backoff, or drops the message when
// shutting down or when the next attempt would fall after its deadline.
func (d *dispatcher) retryLater(it item, now time.Time, draining bool) {
	if draining {
		d.drop(it, dropShutdown)
		return
	}
	it.attempts++
	delay := d.bo.delay(it.attempts)
	if now.Add(delay).After(it.deadline) {
		d.drop(it, dropTTL)
		return
	}
	it.notBefore = now.Add(delay)
	d.requeue(it)
}

func (d *dispatcher) requeue(it item) {
	for _, dr := range d.q.push(it, true) {
		d.report(dr)
	}
}

// recheck reads the current sharing of the message's entity (every line of a
// digest). It fails closed: a message is never sent on a permission it could not read.
func (d *dispatcher) recheck(ctx context.Context, it item) (item, verdict) {
	switch it.msg.Kind {
	case MsgAlert:
		access, err := d.access.Get(ctx, it.msg.entity)
		if err != nil {
			return it, verdictRetry
		}
		if !access.AtLeast(entities.AccessRead) {
			return it, verdictRevoked
		}
	case MsgDigest, MsgStartup:
		kept := make([]DigestLine, 0, len(it.msg.Lines))
		for _, l := range it.msg.Lines {
			access, err := d.access.Get(ctx, l.entity)
			if err != nil {
				return it, verdictRetry
			}
			if access.AtLeast(entities.AccessRead) {
				kept = append(kept, l)
			}
		}
		if len(kept) == 0 {
			return it, verdictRevoked
		}
		it.msg.Lines = kept
	}
	return it, verdictSend
}

// callSend runs the transport call. If the Sender ignores cancellation the call
// is abandoned when ctx ends, so shutdown stays bounded; the abandoned call
// finishes on its own goroutine and is counted.
func (d *dispatcher) callSend(ctx context.Context, m Message) error {
	res := make(chan error, 1)
	go func() { res <- d.sender.Send(ctx, m) }()
	select {
	case err := <-res:
		return err
	case <-ctx.Done():
		d.abandoned.Add(1)
		d.m.abandoned.Add(1)
		return ctx.Err()
	}
}

func (d *dispatcher) drop(it item, r dropReason) {
	d.report(drop{it: it, reason: r})
}

func (d *dispatcher) report(dr drop) {
	d.m.countDrop(dr.reason, d.clock.Now())
	d.log.Warn("notification dropped", "reason", string(dr.reason), "kind", int(dr.it.msg.Kind), "lane", int(dr.it.msg.Lane()))
	d.sink.dropped(dr)
}
