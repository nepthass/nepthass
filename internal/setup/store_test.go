package setup

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/storage"
)

func openStore(t *testing.T, path string) *Store {
	t.Helper()
	db, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db)
}

func TestStore_createGetAndActive_roundTripTheWholeDraft(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "s.db"))
	ctx := context.Background()
	s := fresh()
	s.Draft.Entries["light.a"] = Entry{Desired: Base{Access: entities.AccessControl, Alias: "Lamp"}, Base: Base{Access: entities.AccessRead}}
	s.Draft.View = View{Query: "lamp", Domain: "light", Shared: true, Page: 4, Focus: "light.a", Offered: []entities.ID{"light.a", "light.b"}}

	if err := store.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	byID, errID := store.Get(ctx, "sid")
	active, errActive := store.Active(ctx, "g1", "a1")
	_, otherAdmin := store.Active(ctx, "g1", "someone-else")
	_, otherGuild := store.Active(ctx, "g2", "a1")
	_, missing := store.Get(ctx, "nope")

	if errID != nil || errActive != nil || byID.Draft.Entries["light.a"] != s.Draft.Entries["light.a"] || !reflect.DeepEqual(byID.Draft.View, s.Draft.View) || !byID.CreatedAt.Equal(t0) {
		t.Errorf("round trip lost data: %+v (%v)", byID, errID)
	}
	if active.ID != "sid" || !errors.Is(otherAdmin, ErrNotFound) || !errors.Is(otherGuild, ErrNotFound) || !errors.Is(missing, ErrNotFound) {
		t.Errorf("lookups: active=%q otherAdmin=%v otherGuild=%v missing=%v", active.ID, otherAdmin, otherGuild, missing)
	}
}

func TestStore_allowsOneActiveSessionPerAdministrator_butNotAfterItFinished(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "s.db"))
	ctx := context.Background()
	first := fresh()
	if err := store.Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := newSession("sid2", "g1", "a1", t0)

	dupErr := store.Create(ctx, second)
	other := store.Create(ctx, newSession("sid3", "g1", "a2", t0))
	first.Status = StatusCancelled
	_ = store.Save(ctx, &first)
	afterFinish := store.Create(ctx, second)

	if !errors.Is(dupErr, ErrActiveExists) || other != nil || afterFinish != nil {
		t.Errorf("dup=%v otherAdmin=%v afterFinish=%v", dupErr, other, afterFinish)
	}
}

func TestStore_saveIsACompareAndSwap_andSurvivesRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	store := openStore(t, path)
	ctx := context.Background()
	s := fresh()
	if err := store.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	stale := s // another copy at version 1

	s.Step = StepDiscover
	s.Touch(t0.Add(time.Minute))
	first := store.Save(ctx, &s)
	stale.Step = StepReview
	staleErr := store.Save(ctx, &stale)

	reopened := openStore(t, path)
	got, err := reopened.Get(ctx, "sid")

	if first != nil || s.Version != 2 {
		t.Errorf("first save: %v version=%d", first, s.Version)
	}
	if !errors.Is(staleErr, ErrConflict) || stale.Version != 1 {
		t.Errorf("stale save: %v version=%d, want ErrConflict and an unchanged version", staleErr, stale.Version)
	}
	if err != nil || got.Step != StepDiscover || got.Version != 2 || !got.UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("after restart: %+v (%v)", got, err)
	}
}

func TestStore_concurrentSavesOfTheSameVersion_exactlyOneWins(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "s.db"))
	ctx := context.Background()
	base := fresh()
	if err := store.Create(ctx, base); err != nil {
		t.Fatal(err)
	}
	var wins, conflicts atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := base
			s.Draft = Draft{Entries: map[entities.ID]Entry{}, View: View{Page: i + 1}}
			<-start
			switch err := store.Save(ctx, &s); {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrConflict):
				conflicts.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	got, _ := store.Get(ctx, "sid")
	if wins.Load() != 1 || conflicts.Load() != 11 || got.Version != 2 {
		t.Errorf("wins=%d conflicts=%d version=%d, want 1, 11 and 2", wins.Load(), conflicts.Load(), got.Version)
	}
}

func TestStore_finishedSessionsCannotBeSavedAgain_andArePrunedWhenOld(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "s.db"))
	ctx := context.Background()
	s := fresh()
	_ = store.Create(ctx, s)
	s.Status = StatusCompleted
	if err := store.Save(ctx, &s); err != nil {
		t.Fatal(err)
	}
	again := s
	again.Version = s.Version // the stored version
	reuse := store.Save(ctx, &again)
	activeOld := newSession("old-active", "g9", "a9", t0.Add(-100*24*time.Hour))
	_ = store.Create(ctx, activeOld)

	removed, err := store.Prune(ctx, t0.Add(time.Hour))
	_, completedGone := store.Get(ctx, "sid")
	_, activeKept := store.Get(ctx, "old-active")

	if !errors.Is(reuse, ErrConflict) {
		t.Errorf("saving a completed session: %v, want ErrConflict", reuse)
	}
	if err != nil || removed != 1 || !errors.Is(completedGone, ErrNotFound) || activeKept != nil {
		t.Errorf("prune removed=%d err=%v completedGone=%v activeKept=%v", removed, err, completedGone, activeKept)
	}
}
