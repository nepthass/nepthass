package main

import (
	"sync"
	"testing"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
)

func TestChangeHook_dropsChangesUntilSet_thenForwardsInOrder_andIsRaceFree(t *testing.T) {
	h := &changeHook{}
	h.OnChange(monitor.Change{ID: "x"}) // before the engine exists: dropped, no panic

	var mu sync.Mutex
	var got []uint64
	h.set(func(c monitor.Change) {
		mu.Lock()
		got = append(got, c.Seq)
		mu.Unlock()
	})
	for i := uint64(1); i <= 3; i++ {
		h.OnChange(monitor.Change{ID: entities.ID("x"), Seq: i})
	}
	if len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("changes must be forwarded in order: %v", got)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			h.set(func(monitor.Change) {})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			h.OnChange(monitor.Change{})
		}
	}()
	wg.Wait()
}
