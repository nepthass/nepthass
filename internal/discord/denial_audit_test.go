package discord

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/config"
	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/storage"
)

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// denialEnv drives realistic interactions through real bots that share one audit
// log and one denial recorder, like the single bot of a running process.
type denialEnv struct {
	t     *testing.T
	log   *audit.Log
	fake  *authFake
	logs  *syncBuf
	d     *audit.Denials
	mu    sync.Mutex
	now   time.Time
	count int
	db    interface{ Close() error }
}

func newDenialEnv(t *testing.T) *denialEnv {
	t.Helper()
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "denials.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := &denialEnv{t: t, log: audit.New(db), fake: &authFake{}, logs: &syncBuf{}, now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), db: db}
	e.d = audit.NewDenials(e.log, slog.New(slog.NewTextHandler(e.logs, nil)), func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now })
	return e
}

func (e *denialEnv) advance(d time.Duration) { e.mu.Lock(); e.now = e.now.Add(d); e.mu.Unlock() }

// newBot builds a bot like main does: one recorder and one audit log shared by all.
// New sets discordgo's package-level logger hook, so bots are built one at a time.
func (e *denialEnv) newBot() *Bot {
	e.t.Helper()
	bot, err := New(config.Discord{Token: "discord-token-for-tests", GuildID: guildID, AdminUserIDs: []string{adminID}, AllowedUserIDs: []string{userID}},
		commands.Deps{Notifications: e.fake, Audit: e.log, Denials: e.d, Logger: slog.New(slog.NewTextHandler(e.logs, nil))}, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return bot
}

// serve sends one interaction to a fresh bot (so the per-user command limiter,
// which has its own tests, never interferes) and returns the reply text.
func (e *denialEnv) serve(i *discordgo.Interaction) string { return e.serveOn(e.newBot(), i) }

func (e *denialEnv) serveOn(bot *Bot, i *discordgo.Interaction) string {
	e.t.Helper()
	api := &fakeAPI{}
	if i.ID == "" {
		e.mu.Lock()
		e.count++
		i.ID = fmt.Sprintf("8%018d", e.count)
		e.mu.Unlock()
	}
	bot.serve(context.Background(), api, i)
	return api.finalText()
}

func (e *denialEnv) rows() []audit.Entry {
	e.t.Helper()
	rows, err := e.log.Recent(context.Background(), 10000)
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func (e *denialEnv) denials() []audit.Entry {
	var out []audit.Entry
	for _, r := range e.rows() {
		if r.Outcome == audit.OutcomeDenied {
			out = append(out, r)
		}
	}
	return out
}

func adminCommands() map[string][]*discordgo.ApplicationCommandInteractionDataOption {
	return map[string][]*discordgo.ApplicationCommandInteractionDataOption{
		"permissions":   {sub("allow", str("entity", "binary_sensor.secret_door"), str("access", "control"))},
		"audit":         nil,
		"setup":         nil,
		"sync":          nil,
		"notifications": {sub("add", str("entity", "binary_sensor.secret_door"))},
	}
}

func TestDenialAudit_everyIdentityAgainstEveryAdministratorCommand(t *testing.T) {
	e := newDenialEnv(t)
	other := "600000000000000009"
	identities := []struct {
		name   string
		guild  string
		user   string
		reason string
		want   string // part of the reply the caller sees
	}{
		{"an allowed user who is not an administrator", guildID, userID, "reason=not_admin", "administrators"},
		{"a stranger in the configured guild", guildID, strangerID, "reason=not_allowed", "not authorized"},
		{"an administrator ID used in another guild", other, adminID, "reason=wrong_guild guild=" + other, "not authorized"},
		{"a stranger in another guild", other, strangerID, "reason=wrong_guild guild=" + other, "not authorized"},
		{"an administrator ID in a direct message", "", adminID, "reason=wrong_guild guild=none", "not authorized"},
	}
	for _, who := range identities {
		before := len(e.denials())
		for name, opts := range adminCommands() {
			e.advance(2 * time.Minute) // keeps this matrix clear of the per-actor limit, which has its own test
			reply := strings.ToLower(e.serve(slash(who.guild, who.user, name, opts...)))
			if !strings.Contains(reply, who.want) {
				t.Errorf("%s running %s: reply %q must still say %q", who.name, name, reply, who.want)
			}
			if strings.Contains(reply, "secret_door") {
				t.Errorf("%s running %s: the reply must not echo the entity", who.name, name)
			}
		}
		rows := e.denials()[:len(e.denials())-before]
		if len(rows) != len(adminCommands()) {
			t.Fatalf("%s: %d rows for %d refused commands", who.name, len(rows), len(adminCommands()))
		}
		for _, r := range rows {
			if r.ActorID != who.user || r.Detail != who.reason || r.EntityID != "" || r.Outcome != audit.OutcomeDenied {
				t.Errorf("%s: row %+v, want actor %s detail %q and no entity", who.name, r, who.user, who.reason)
			}
		}
	}
	if e.fake.n() != 0 {
		t.Fatalf("not one refused call may reach the notification service: %d", e.fake.n())
	}
	for _, r := range e.rows() {
		if strings.Contains(r.Detail+r.Action+r.Command+r.EntityID, "secret_door") {
			t.Errorf("an option value reached the audit log: %+v", r)
		}
	}
	if strings.Contains(e.logs.String(), "secret_door") {
		t.Error("an option value reached the logs")
	}
}

func TestDenialAudit_theAdministratorIsNeverRecordedAndStillServed(t *testing.T) {
	e := newDenialEnv(t)
	e.serve(slash(guildID, adminID, "notifications", sub("status")))
	if e.fake.n() != 1 || len(e.denials()) != 0 {
		t.Fatalf("an administrator is served and is not a denial: calls=%d rows=%d", e.fake.n(), len(e.denials()))
	}
}

func TestDenialAudit_onlyAdministratorCommandsFromRefusedCallersAreRecorded(t *testing.T) {
	e := newDenialEnv(t)
	e.serve(slash(guildID, strangerID, "ping"))                               // not an administrator command
	e.serve(slash(guildID, strangerID, "evil"))                               // a name NeptHass never defined
	e.serve(slash(guildID, strangerID, "notifications; DROP TABLE users"))    // a hostile name
	e.serve(slash(guildID, userID, "ping"))                                   // allowed and allowed
	e.serve(complete(guildID, strangerID, "notifications", "remove", "rule")) // autocomplete
	e.serve(complete(guildID, userID, "notifications", "remove", "rule"))     // autocomplete
	e.serve(&discordgo.Interaction{Type: discordgo.InteractionMessageComponent, GuildID: guildID, Member: &discordgo.Member{User: &discordgo.User{ID: strangerID}}})
	e.serve(&discordgo.Interaction{Type: discordgo.InteractionModalSubmit, GuildID: guildID, Member: &discordgo.Member{User: &discordgo.User{ID: strangerID}}})
	if rows := e.denials(); len(rows) != 0 {
		t.Fatalf("none of these is a refused administrator command: %+v", rows)
	}
}

func TestDenialAudit_malformedInteractionsAreRefusedWithoutPanicAndWithoutARow(t *testing.T) {
	e := newDenialEnv(t)
	for name, i := range map[string]*discordgo.Interaction{
		"no data":          {Type: discordgo.InteractionApplicationCommand, GuildID: guildID, Member: &discordgo.Member{User: &discordgo.User{ID: strangerID}}},
		"no member":        {Type: discordgo.InteractionApplicationCommand, GuildID: guildID, Data: discordgo.ApplicationCommandInteractionData{Name: "notifications"}},
		"unknown type":     {Type: discordgo.InteractionType(99), GuildID: guildID, Member: &discordgo.Member{User: &discordgo.User{ID: strangerID}}},
		"a nil subcommand": {Type: discordgo.InteractionApplicationCommand, GuildID: guildID, Member: &discordgo.Member{User: &discordgo.User{ID: strangerID}}, Data: discordgo.ApplicationCommandInteractionData{Name: "notifications", Options: []*discordgo.ApplicationCommandInteractionDataOption{nil}}},
		"a bad user id":    slash(guildID, "<@everyone>", "notifications", sub("add")),
	} {
		e.advance(2 * time.Minute)
		_ = e.serve(i) // must not panic (serve also recovers, and the bot must stay up)
		_ = name
	}
	rows := e.denials()
	if len(rows) != 3 {
		t.Fatalf("a missing member, a bad user ID and a nil subcommand are refusals of an administrator command; the others have no command to name: %+v", rows)
	}
	for _, r := range rows {
		if (r.ActorID != "unknown" && r.ActorID != strangerID) || strings.ContainsAny(r.ActorID+r.Detail+r.Action, "<>@") {
			t.Errorf("a malformed identity is recorded as a fixed word, never raw: %+v", r)
		}
	}
	if e.fake.n() != 0 {
		t.Fatal("a malformed interaction must never reach a service")
	}
}

func TestDenialAudit_aReplayedInteractionIsOneRow(t *testing.T) {
	e := newDenialEnv(t)
	i := slash(guildID, userID, "notifications", sub("add"))
	i.ID = "9100000000000000001"
	e.serve(i)
	e.advance(10 * time.Minute)
	j := slash(guildID, userID, "notifications", sub("add"))
	j.ID = "9100000000000000001" // the very same interaction delivered again
	e.serve(j)
	if n := len(e.denials()); n != 1 {
		t.Fatalf("a replay is not a second attempt: %d rows", n)
	}
}

func TestDenialAudit_aFloodIsBoundedAndTheNextRowSaysHowManyWereDropped(t *testing.T) {
	e := newDenialEnv(t)
	for i := 0; i < 300; i++ {
		e.serve(slash(guildID, strangerID, "notifications", sub("add"))) // one stranger, the same minute
	}
	if n := len(e.denials()); n != audit.DenialBurst {
		t.Fatalf("a stranger hammering an administrator command writes %d rows, not 300: %d", audit.DenialBurst, n)
	}
	for i := 0; i < 200; i++ { // many strangers at once
		e.serve(slash(guildID, fmt.Sprintf("5000000000%08d", i), "audit"))
	}
	if n := len(e.denials()); n > audit.DenialsPerHour {
		t.Fatalf("all callers together are capped per hour: %d rows", n)
	}
	e.advance(61 * time.Minute)
	e.serve(slash(guildID, strangerID, "notifications", sub("add")))
	dropped := false
	for _, r := range e.denials() {
		dropped = dropped || strings.Contains(r.Detail, "suppressed=")
	}
	if !dropped {
		t.Error("the first row after the cap says how many attempts were not recorded")
	}
}

func TestDenialAudit_aFailingAuditDatabaseChangesNothingAboutTheDecision(t *testing.T) {
	e := newDenialEnv(t)
	userBefore := strings.ToLower(e.serve(slash(guildID, userID, "notifications", sub("add"))))
	strangerBefore := strings.ToLower(e.serve(slash(guildID, strangerID, "permissions", sub("allow"))))
	_ = e.db.Close() // every audit write now fails
	e.advance(2 * time.Minute)
	if got := strings.ToLower(e.serve(slash(guildID, userID, "notifications", sub("add")))); got != userBefore {
		t.Errorf("a failing audit must not change the reply: %q vs %q", got, userBefore)
	}
	e.advance(2 * time.Minute)
	if got := strings.ToLower(e.serve(slash(guildID, strangerID, "permissions", sub("allow")))); got != strangerBefore {
		t.Errorf("a failing audit must not change the reply: %q vs %q", got, strangerBefore)
	}
	if e.fake.n() != 0 {
		t.Fatal("a failing audit must never turn a refusal into access")
	}
	if !strings.Contains(e.logs.String(), "could not audit a refused command") {
		t.Error("the failure is logged")
	}
	if got := e.serve(slash(guildID, adminID, "notifications", sub("status"))); e.fake.n() != 1 {
		t.Errorf("an administrator is still served while the audit database is failing: %q", got)
	}
}

func TestDenialAudit_concurrentDenialsStayBounded(t *testing.T) {
	e := newDenialEnv(t)
	bots := make([]*Bot, 40)
	for w := range bots {
		bots[w] = e.newBot()
	}
	var wg sync.WaitGroup
	for w := 0; w < 40; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				who := strangerID
				if w%2 == 0 {
					who = userID
				}
				e.serveOn(bots[w], slash(guildID, who, "notifications", sub("add")))
			}
		}(w)
	}
	wg.Wait()
	if n := len(e.denials()); n > 2*audit.DenialBurst {
		t.Fatalf("two actors may write at most %d rows between them: %d", 2*audit.DenialBurst, n)
	}
	if e.fake.n() != 0 {
		t.Fatal("no concurrent refusal reaches a service")
	}
}
