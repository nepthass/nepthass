package audit

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/storage"
)

func newLog(t *testing.T) *Log {
	t.Helper()
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db)
}

var base = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func TestRecentReturnsNewestFirst_andPreservesFields(t *testing.T) {
	log := newLog(t)
	ctx := context.Background()
	entries := []Entry{
		{Time: base, ActorID: "1", Command: "control", EntityID: "light.a", Action: "on", Outcome: OutcomeSuccess},
		{Time: base.Add(time.Minute), ActorID: "2", Command: "permissions.allow", Action: "read", Outcome: OutcomeDenied, Detail: "nope"},
	}
	for _, e := range entries {
		if err := log.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	got, err := log.Recent(ctx, 10)

	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != entries[1] || got[1] != entries[0] {
		t.Errorf("Recent() = %+v", got)
	}
}

func TestRecentHonoursLimit(t *testing.T) {
	log := newLog(t)
	for i := range 5 {
		if err := log.Record(context.Background(), Entry{Time: base.Add(time.Duration(i) * time.Second), ActorID: "1", Command: "c", Action: "a", Outcome: OutcomeSuccess}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := log.Recent(context.Background(), 3)

	if err != nil || len(got) != 3 {
		t.Errorf("len=%d err=%v", len(got), err)
	}
}

func TestRecordTruncatesLongDetail(t *testing.T) {
	log := newLog(t)
	if err := log.Record(context.Background(), Entry{Time: base, ActorID: "1", Command: "c", Action: "a", Outcome: OutcomeError, Detail: strings.Repeat("x", 5000)}); err != nil {
		t.Fatal(err)
	}

	got, err := log.Recent(context.Background(), 1)

	if err != nil || len(got[0].Detail) != maxDetailLen {
		t.Errorf("detail length = %d err=%v", len(got[0].Detail), err)
	}
}

func TestPruneRemovesOnlyEntriesOlderThanCutoff(t *testing.T) {
	log := newLog(t)
	ctx := context.Background()
	for _, at := range []time.Time{base.Add(-48 * time.Hour), base.Add(-time.Hour), base} {
		if err := log.Record(ctx, Entry{Time: at, ActorID: "1", Command: "c", Action: "a", Outcome: OutcomeSuccess}); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := log.Prune(ctx, base.Add(-24*time.Hour))

	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	left, _ := log.Recent(ctx, 10)
	if len(left) != 2 {
		t.Errorf("entries left = %d, want 2", len(left))
	}
}
