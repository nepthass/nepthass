package monitor

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
)

func chg(seq uint64) Change { return Change{ID: entities.ID("sensor.x"), Seq: seq} }

// within fails the test if f does not return in time: a blocked Publish would hang the monitor.
func within(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked", what)
	}
}

func TestHub_deliversEveryChangeInOrderToEverySubscriber(t *testing.T) {
	h := NewHub()
	a, _ := h.Subscribe(100)
	b, _ := h.Subscribe(100)
	for i := uint64(1); i <= 100; i++ {
		h.Publish(chg(i))
	}
	for _, s := range []*Subscription{a, b} {
		for i := uint64(1); i <= 100; i++ {
			if got := (<-s.C).Seq; got != i {
				t.Fatalf("out of order: got %d, want %d", got, i)
			}
		}
	}
}

func TestHub_aSlowSubscriberLosesChangesButNeverBlocksThePublisherOrOthers(t *testing.T) {
	h := NewHub()
	slow, _ := h.Subscribe(1) // never read
	fast, _ := h.Subscribe(10)
	within(t, "Publish", func() {
		for i := uint64(1); i <= 5; i++ {
			h.Publish(chg(i))
		}
	})
	if slow.Dropped() != 4 {
		t.Errorf("slow subscriber dropped %d, want 4", slow.Dropped())
	}
	if fast.Dropped() != 0 || len(fast.C) != 5 {
		t.Errorf("fast subscriber must receive everything: dropped=%d queued=%d", fast.Dropped(), len(fast.C))
	}
}

func TestHub_closeDetachesClosesTheChannelAndIsIdempotent(t *testing.T) {
	h := NewHub()
	s, _ := h.Subscribe(1)
	s.Close()
	s.Close()
	if _, open := <-s.C; open {
		t.Error("the channel must be closed")
	}
	if h.Len() != 0 {
		t.Errorf("Len %d after Close", h.Len())
	}
	within(t, "Publish after Close", func() { h.Publish(chg(1)) }) // must not panic on a closed channel
}

func TestHub_isBounded_andAClosedSlotCanBeReused(t *testing.T) {
	h := NewHub()
	var subs []*Subscription
	for i := 0; i < MaxSubscribers; i++ {
		s, err := h.Subscribe(1)
		if err != nil {
			t.Fatalf("subscriber %d: %v", i, err)
		}
		subs = append(subs, s)
	}
	if _, err := h.Subscribe(1); !errors.Is(err, ErrTooManySubscribers) {
		t.Fatalf("want ErrTooManySubscribers, got %v", err)
	}
	subs[0].Close()
	if _, err := h.Subscribe(1); err != nil {
		t.Errorf("a freed slot must be reusable: %v", err)
	}
}

func TestHub_withoutSubscribersPublishIsANoOp(t *testing.T) {
	within(t, "Publish", func() { NewHub().Publish(chg(1)) })
}

func TestHub_isSafeForConcurrentPublishSubscribeAndClose(t *testing.T) {
	h := NewHub()
	stop := make(chan struct{})
	var publisher, closers sync.WaitGroup
	publisher.Add(1)
	go func() {
		defer publisher.Done()
		for i := uint64(1); ; i++ {
			select {
			case <-stop:
				return
			default:
				h.Publish(chg(i))
			}
		}
	}()
	for g := 0; g < 8; g++ {
		closers.Add(1)
		go func() {
			defer closers.Done()
			for i := 0; i < 200; i++ {
				s, err := h.Subscribe(2)
				if err != nil {
					continue // the bound is full: another goroutine holds the slots
				}
				select {
				case <-s.C:
				default:
				}
				s.Close()
			}
		}()
	}
	within(t, "subscribe and close under publish", closers.Wait)
	close(stop)
	within(t, "publisher shutdown", publisher.Wait)
	if h.Len() != 0 {
		t.Errorf("%d subscribers left attached", h.Len())
	}
}

func TestHub_clampsTheSubscriberBuffer(t *testing.T) {
	h := NewHub()
	lo, _ := h.Subscribe(0)
	hi, _ := h.Subscribe(1 << 20)
	if cap(lo.ch) != 1 || cap(hi.ch) != maxSubscriberBuffer {
		t.Errorf("buffers %d and %d, want 1 and %d", cap(lo.ch), cap(hi.ch), maxSubscriberBuffer)
	}
}
