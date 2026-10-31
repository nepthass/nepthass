package main

import (
	"testing"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
)

// The hub is added next to the engine path: the engine still receives every change in order, and a
// subscriber receives the same stream.
func TestChangeHook_feedsTheEngineExactlyAsBefore_andTheHubAlongside(t *testing.T) {
	hub := monitor.NewHub()
	sub, err := hub.Subscribe(8)
	if err != nil {
		t.Fatal(err)
	}
	h := &changeHook{hub: hub}
	var engineSaw []uint64
	h.set(func(c monitor.Change) { engineSaw = append(engineSaw, c.Seq) })
	for i := uint64(1); i <= 3; i++ {
		h.OnChange(monitor.Change{ID: entities.ID("sensor.x"), Seq: i})
	}
	for i := uint64(1); i <= 3; i++ {
		// OnChange is synchronous, so the change is already queued: no waiting, and a missing
		// publish fails here instead of hanging.
		select {
		case c := <-sub.C:
			if c.Seq != i {
				t.Errorf("hub got %d, want %d", c.Seq, i)
			}
		default:
			t.Fatalf("the hub did not receive change %d", i)
		}
		if engineSaw[i-1] != i {
			t.Errorf("engine got %v", engineSaw)
		}
	}
	// no hub and no engine yet: dropping silently is the documented behavior
	(&changeHook{}).OnChange(monitor.Change{Seq: 9})
}

// A panic in the engine callback is recovered by the monitor; it must not cost a subscriber the
// change, which is why the hub is fed first.
func TestChangeHook_aPanickingEngineCallbackDoesNotCostASubscriberTheChange(t *testing.T) {
	hub := monitor.NewHub()
	sub, _ := hub.Subscribe(2)
	h := &changeHook{hub: hub}
	h.set(func(monitor.Change) { panic("engine") })
	func() {
		defer func() { _ = recover() }()
		h.OnChange(monitor.Change{ID: entities.ID("sensor.x"), Seq: 1})
	}()
	select {
	case c := <-sub.C:
		if c.Seq != 1 {
			t.Errorf("hub got %d, want 1", c.Seq)
		}
	default:
		t.Fatal("the subscriber missed the change")
	}
}
