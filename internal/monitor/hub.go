package monitor

import (
	"errors"
	"sync"
	"sync/atomic"
)

// MaxSubscribers bounds the hub so a buggy or hostile consumer cannot grow memory.
const MaxSubscribers = 64

const maxSubscriberBuffer = 1024

// ErrTooManySubscribers is returned by Subscribe when MaxSubscribers are already attached.
var ErrTooManySubscribers = errors.New("monitor: too many change subscribers")

// Hub fans the monitor's single Change stream out to any number of consumers (for example a
// browser event stream) without touching the existing in-process consumer.
//
// Publish honors the Config.OnChange contract: it is constant-time, never blocks on a consumer and
// performs no I/O. It can only wait for the few instructions of a concurrent Subscribe or Close
// (a map insert or delete and a channel close), never for an allocation or a consumer. The hub starts
// no goroutines, and its memory is bounded by MaxSubscribers times the buffer size. A subscriber that does not keep up loses changes (counted by Dropped) instead of slowing
// the monitor; the consumer is expected to resynchronize from a snapshot when Dropped moves.
// A Hub never reads or filters Change contents: the consumer must apply authorization to every
// change it forwards, because sharing can be revoked at any time.
type Hub struct {
	mu   sync.RWMutex
	subs map[*Subscription]struct{}
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{subs: make(map[*Subscription]struct{})} }

// Subscription is one consumer's ordered view of the change stream.
type Subscription struct {
	// C delivers changes in publication order; it is closed by Close.
	C <-chan Change

	ch      chan Change
	hub     *Hub
	dropped atomic.Uint64
	once    sync.Once
}

// Subscribe attaches a consumer with a buffer of the given size (clamped to 1..1024).
func (h *Hub) Subscribe(buffer int) (*Subscription, error) {
	buffer = min(max(buffer, 1), maxSubscriberBuffer)
	// The buffer is allocated before the lock is taken, so Publish never waits on an allocation.
	ch := make(chan Change, buffer)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= MaxSubscribers {
		return nil, ErrTooManySubscribers
	}
	s := &Subscription{C: ch, ch: ch, hub: h}
	h.subs[s] = struct{}{}
	return s, nil
}

// Publish hands c to every subscriber without ever blocking.
func (h *Hub) Publish(c Change) {
	h.mu.RLock()
	for s := range h.subs {
		select {
		case s.ch <- c:
		default:
			s.dropped.Add(1)
		}
	}
	h.mu.RUnlock()
}

// Len reports the number of attached subscribers.
func (h *Hub) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// Dropped counts the changes this subscriber lost because its buffer was full.
func (s *Subscription) Dropped() uint64 { return s.dropped.Load() }

// Close detaches the subscriber and closes C. It is safe to call more than once.
func (s *Subscription) Close() {
	s.once.Do(func() {
		s.hub.mu.Lock()
		delete(s.hub.subs, s)
		close(s.ch)
		s.hub.mu.Unlock()
	})
}
