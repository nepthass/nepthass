package notify

import (
	"sync"
	"time"
)

type dropReason string

const (
	dropOverflow  dropReason = "overflow"
	dropTTL       dropReason = "ttl"
	dropRevoked   dropReason = "revoked"
	dropPermanent dropReason = "permanent"
	dropBreaker   dropReason = "breaker"
	dropDisabled  dropReason = "disabled"
	dropShutdown  dropReason = "shutdown"
	// dropCancelled: withdrawn because it became stale (a down alert whose outage ended before delivery).
	dropCancelled dropReason = "cancelled"
)

// item is a queued message with its absolute deadline: the TTL runs from the
// moment the qualifying state was observed and is not extended by retries.
type item struct {
	msg       Message
	deadline  time.Time
	notBefore time.Time
	attempts  int
}

type drop struct {
	it     item
	reason dropReason
}

// queue holds two bounded FIFO lanes. Messages of one rule share a lane, so a
// rule's alerts keep their order within it; a critical message may overtake a
// normal one (and a message waiting to retry may be overtaken by later ones),
// which is the documented ordering limit of the priority lanes.
//
// When a lane is full something is dropped, never silently: the caller receives
// every drop. The normal lane drops its oldest message. The critical lane drops
// the lowest-priority, oldest message, but only if the newcomer is at least as
// important; an active leak alert is therefore never evicted for a digest, a
// recovery notice or monitoring-down.
type queue struct {
	mu      sync.Mutex
	crit    []item
	norm    []item
	capCrit int
	capNorm int
	wake    chan struct{}
}

func newQueue(capCrit, capNorm int) *queue {
	return &queue{capCrit: capCrit, capNorm: capNorm, wake: make(chan struct{}, 1)}
}

func (q *queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// push adds it (at the front of its lane when front is set, for a retry) and
// returns what was dropped to make room, which may be it itself.
func (q *queue) push(it item, front bool) []drop {
	q.mu.Lock()
	lane, limit := &q.norm, q.capNorm
	if it.msg.Lane() == LaneCritical {
		lane, limit = &q.crit, q.capCrit
	}
	var dropped []drop
	if len(*lane) >= limit {
		victim := 0
		if it.msg.Lane() == LaneCritical {
			for i, c := range *lane {
				if c.msg.priority() < (*lane)[victim].msg.priority() {
					victim = i
				}
			}
			if it.msg.priority() < (*lane)[victim].msg.priority() {
				q.mu.Unlock()
				return []drop{{it: it, reason: dropOverflow}}
			}
		}
		dropped = append(dropped, drop{it: (*lane)[victim], reason: dropOverflow})
		*lane = append((*lane)[:victim], (*lane)[victim+1:]...)
	}
	if front {
		*lane = append([]item{it}, *lane...)
	} else {
		*lane = append(*lane, it)
	}
	q.mu.Unlock()
	q.signal()
	return dropped
}

// pop returns the first eligible message, critical lane first. When nothing is
// eligible, next is the earliest time one becomes eligible (zero if the queue is
// empty).
func (q *queue) pop(now time.Time) (it item, ok bool, next time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, lane := range []*[]item{&q.crit, &q.norm} {
		for i, c := range *lane {
			if !c.notBefore.After(now) {
				*lane = append((*lane)[:i], (*lane)[i+1:]...)
				return c, true, time.Time{}
			}
		}
	}
	for _, lane := range [][]item{q.crit, q.norm} {
		for _, c := range lane {
			if next.IsZero() || c.notBefore.Before(next) {
				next = c.notBefore
			}
		}
	}
	return item{}, false, next
}

// popAny returns the next message regardless of its retry time (used to drain
// at shutdown).
func (q *queue) popAny() (item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, lane := range []*[]item{&q.crit, &q.norm} {
		if len(*lane) > 0 {
			c := (*lane)[0]
			*lane = (*lane)[1:]
			return c, true
		}
	}
	return item{}, false
}

func (q *queue) depths() (crit, norm int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.crit), len(q.norm)
}

// removeWhere takes out every queued message matching pred and returns them.
func (q *queue) removeWhere(pred func(item) bool) []item {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []item
	for _, lane := range []*[]item{&q.crit, &q.norm} {
		kept := (*lane)[:0]
		for _, c := range *lane {
			if pred(c) {
				out = append(out, c)
			} else {
				kept = append(kept, c)
			}
		}
		*lane = kept
	}
	return out
}
