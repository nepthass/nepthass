package audit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/storage"
)

var ctx = context.Background()

type denialFix struct {
	t   *testing.T
	log *Log
	d   *Denials
	now time.Time
	mu  sync.Mutex
}

func newDenialFix(t *testing.T) *denialFix {
	t.Helper()
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := &denialFix{t: t, log: New(db), now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	f.d = NewDenials(f.log, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	})
	return f
}

func (f *denialFix) advance(d time.Duration) { f.mu.Lock(); f.now = f.now.Add(d); f.mu.Unlock() }

func (f *denialFix) rows() []Entry {
	f.t.Helper()
	rows, err := f.log.Recent(context.Background(), 10000)
	if err != nil {
		f.t.Fatal(err)
	}
	return rows
}

func attempt(actor string, reason Reason) DenialAttempt {
	return DenialAttempt{ActorID: actor, Command: "notifications", Action: "add", GuildID: "100000000000000001", Reason: reason}
}

func TestDenials_recordsOnlyNamesAndAFixedReason(t *testing.T) {
	f := newDenialFix(t)
	f.d.Record(ctx, attempt("500000000000000003", ReasonNotAdmin))
	f.d.Record(ctx, DenialAttempt{ActorID: "500000000000000004", Command: "permissions", Reason: ReasonNotAllowed})
	f.d.Record(ctx, DenialAttempt{ActorID: "500000000000000005", Command: "audit", GuildID: "999000000000000001", Reason: ReasonWrongGuild})
	f.d.Record(ctx, DenialAttempt{ActorID: "500000000000000006", Command: "sync", GuildID: "", Reason: ReasonWrongGuild})
	rows := f.rows()
	if len(rows) != 4 {
		t.Fatalf("rows = %+v", rows)
	}
	want := map[string]string{"500000000000000003": "reason=not_admin", "500000000000000004": "reason=not_allowed",
		"500000000000000005": "reason=wrong_guild guild=999000000000000001", "500000000000000006": "reason=wrong_guild guild=none"}
	for _, e := range rows {
		if e.Outcome != OutcomeDenied || e.EntityID != "" || e.Detail != want[e.ActorID] {
			t.Errorf("row %+v, want detail %q", e, want[e.ActorID])
		}
	}
	kept := false
	for _, e := range rows {
		kept = kept || e.Action == "add"
	}
	if !kept {
		t.Error("the subcommand name is kept")
	}
}

func TestDenials_untrustedStringsAreValidatedNeverStoredRaw(t *testing.T) {
	f := newDenialFix(t)
	for _, a := range []DenialAttempt{
		{ActorID: "500000000000000003", Command: "notifications; DROP TABLE x", Reason: ReasonNotAdmin},
		{ActorID: "500000000000000003", Command: "Notifications", Reason: ReasonNotAdmin},
		{ActorID: "500000000000000003", Command: strings.Repeat("a", 33), Reason: ReasonNotAdmin},
		{ActorID: "500000000000000003", Command: "", Reason: ReasonNotAdmin},
		{ActorID: "500000000000000003", Command: "notifications", Reason: "because I said so"},
		{ActorID: "500000000000000003", Command: "notifications", Reason: ""},
	} {
		f.d.Record(ctx, a)
	}
	if n := len(f.rows()); n != 0 {
		t.Fatalf("nothing unsafe may become a row: %d", n)
	}
	f.d.Record(ctx, DenialAttempt{ActorID: "<@123> @everyone", Command: "notifications", Action: "add\nnow", GuildID: "'; DROP", Reason: ReasonWrongGuild})
	rows := f.rows()
	if len(rows) != 1 || rows[0].ActorID != "unknown" || rows[0].Action != "" || rows[0].Detail != "reason=wrong_guild guild=none" {
		t.Fatalf("a hostile actor, subcommand and guild are replaced by fixed words: %+v", rows)
	}
	var nilRecorder *Denials
	nilRecorder.Record(ctx, attempt("500000000000000003", ReasonNotAdmin)) // must be a no-op, not a panic
}

func TestDenials_aReplayedInteractionIsRecordedOnce(t *testing.T) {
	f := newDenialFix(t)
	a := attempt("500000000000000003", ReasonNotAdmin)
	a.InteractionID = "9000000000000000001"
	f.d.Record(ctx, a)
	f.d.Record(ctx, a)
	f.advance(time.Hour)
	f.d.Record(ctx, a)
	if n := len(f.rows()); n != 1 {
		t.Fatalf("a replay is not a second attempt: %d rows", n)
	}
	for i := 0; i < maxSeen+10; i++ { // the memory of replays is bounded
		b := attempt(fmt.Sprintf("5000000000000%05d", i), ReasonNotAdmin)
		b.InteractionID = fmt.Sprintf("8%018d", i)
		f.d.Record(ctx, b)
		f.advance(time.Minute)
	}
	if len(f.d.seen) > maxSeen || len(f.d.ring) > maxSeen {
		t.Fatalf("the replay memory must stay bounded: %d/%d", len(f.d.seen), len(f.d.ring))
	}
}

func TestDenials_oneActorIsLimitedToABurstThenOnePerMinute(t *testing.T) {
	f := newDenialFix(t)
	for i := 0; i < 50; i++ {
		f.d.Record(ctx, attempt("500000000000000003", ReasonNotAdmin))
	}
	if n := len(f.rows()); n != DenialBurst {
		t.Fatalf("a flood from one actor writes %d rows, not 50: %d", DenialBurst, n)
	}
	f.advance(90 * time.Second) // one token refilled
	f.d.Record(ctx, attempt("500000000000000003", ReasonNotAdmin))
	f.d.Record(ctx, attempt("500000000000000003", ReasonNotAdmin))
	rows := f.rows()
	if len(rows) != DenialBurst+1 {
		t.Fatalf("one more row after a minute: %d", len(rows))
	}
	counted := false
	for _, e := range rows {
		counted = counted || strings.Contains(e.Detail, "suppressed=47")
	}
	if !counted {
		t.Errorf("the dropped attempts are counted in the next row: %+v", rows)
	}
}

func TestDenials_allActorsTogetherAreCappedPerHour_andTheCapResets(t *testing.T) {
	f := newDenialFix(t)
	for i := 0; i < 200; i++ {
		f.d.Record(ctx, attempt(fmt.Sprintf("5000000000000%05d", i), ReasonNotAllowed))
	}
	if n := len(f.rows()); n != DenialsPerHour {
		t.Fatalf("the global cap holds against many strangers: %d rows", n)
	}
	f.advance(time.Hour + time.Second)
	f.d.Record(ctx, attempt("500000000000099999", ReasonNotAllowed))
	rows := f.rows()
	counted := false
	for _, e := range rows {
		counted = counted || strings.Contains(e.Detail, "suppressed=140")
	}
	if len(rows) != DenialsPerHour+1 || !counted {
		t.Fatalf("after an hour the cap resets and the row says how many were dropped: %d rows", len(rows))
	}
}

func TestDenials_concurrentCallersStayWithinTheLimits(t *testing.T) {
	f := newDenialFix(t)
	var wg sync.WaitGroup
	for w := 0; w < 40; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				a := attempt(fmt.Sprintf("5000000000000%05d", w%8), ReasonNotAdmin)
				a.InteractionID = fmt.Sprintf("7%018d", w*100+i)
				f.d.Record(ctx, a)
			}
		}(w)
	}
	wg.Wait()
	if n := len(f.rows()); n > 8*DenialBurst || n > DenialsPerHour {
		t.Fatalf("eight actors may write at most %d rows between them: %d", 8*DenialBurst, n)
	}
}

func TestDenials_aFailingAuditDatabaseIsLoggedAndHarmless(t *testing.T) {
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "gone.db"))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	d := NewDenials(New(db), slog.New(slog.NewTextHandler(&out, nil)), nil)
	_ = db.Close() // every write now fails
	d.Record(ctx, attempt("500000000000000003", ReasonNotAdmin))
	if !strings.Contains(out.String(), "could not audit a refused command") {
		t.Errorf("a failed audit write must be logged: %q", out.String())
	}
	if strings.Contains(out.String(), "500000000000000003") {
		t.Error("the log line carries no user data beyond the command name")
	}
}

func TestPruneDenied_keepsTheNewestDenialRowsAndNothingElse(t *testing.T) {
	f := newDenialFix(t)
	insert := func(outcome, detail string, n int) {
		for i := 0; i < n; i++ {
			if err := f.log.Record(ctx, Entry{Time: f.now, ActorID: "1", Command: "c", Action: "a", Outcome: Outcome(outcome), Detail: detail}); err != nil {
				t.Fatal(err)
			}
		}
	}
	insert("denied", "reason=not_admin", 30)
	insert("denied", "administrator access required", 5) // a denial written by the services: never pruned here
	insert("success", "reason=looks-similar", 5)
	insert("denied", "reason=not_allowed", 10)
	n, err := f.log.PruneDenied(ctx, 25)
	if err != nil || n != 15 {
		t.Fatalf("40 recorded denials, keep 25: removed %d, %v", n, err)
	}
	counts := map[string]int{}
	for _, e := range f.rows() {
		counts[string(e.Outcome)+"|"+e.Detail]++
	}
	if counts["denied|reason=not_admin"]+counts["denied|reason=not_allowed"] != 25 || counts["denied|administrator access required"] != 5 || counts["success|reason=looks-similar"] != 5 {
		t.Fatalf("only the oldest denial rows go: %v", counts)
	}
	if got := counts["denied|reason=not_allowed"]; got != 10 {
		t.Errorf("the newest rows are the ones kept: %d", got)
	}
}
