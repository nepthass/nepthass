package wizard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/homeassistant/rest"
	"github.com/nepthass/nepthass/internal/setup"
	"github.com/nepthass/nepthass/internal/storage"
)

const guild = "g1"

var escapedPair = regexp.MustCompile(`\\.`)

func nopLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

var (
	admin   = auth.Actor{UserID: "a1", Level: auth.LevelAdmin}
	admin2  = auth.Actor{UserID: "a2", Level: auth.LevelAdmin}
	regular = auth.Actor{UserID: "u1", Level: auth.LevelUser}
)

type fakeHA struct {
	mu     sync.Mutex
	states map[entities.ID]entities.State
	err    error
}

func (f *fakeHA) States(context.Context) ([]entities.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([]entities.State, 0, len(f.states))
	for _, s := range f.states {
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeHA) put(id, name, state string, attrs map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if attrs == nil {
		attrs = map[string]any{}
	}
	attrs["friendly_name"] = name
	f.states[entities.ID(id)] = entities.State{ID: entities.ID(id), State: state, Attributes: attrs}
}

type health struct{ rep commands.HealthReport }

func (h *health) Probe(context.Context) commands.HealthReport { return h.rep }

type env struct {
	ui      *UI
	svc     *setup.Service
	ha      *fakeHA
	perms   *entities.Permissions
	aliases *entities.Aliases
	health  *health
	clock   *time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "wizard.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	ha := &fakeHA{states: map[entities.ID]entities.State{}}
	perms, aliases := entities.NewPermissions(db), entities.NewAliases(db)
	disc := entities.NewDiscovery(entities.DiscoveryDeps{HA: ha, DB: db, Permissions: perms, Now: now})
	svc := setup.NewService(setup.Deps{DB: db, Store: setup.NewStore(db), Discovery: disc, Permissions: perms, Aliases: aliases, Audit: audit.New(db), Now: now})
	h := &health{rep: commands.HealthReport{Gateway: commands.Gateway{Connected: true}, HAVersion: "2026.10.4"}}
	e := &env{svc: svc, ha: ha, perms: perms, aliases: aliases, health: h, clock: &clock}
	e.ui = New(Deps{Service: svc, Prober: h, Version: "test", Logger: nopLogger()})
	return e
}

// found is a component located in a reply.
type found struct {
	customID string
	disabled bool
	options  []string
}

// find locates the first component with the given action in a reply.
func find(r commands.Reply, act action) (found, bool) {
	for _, c := range r.Components {
		row, _ := c.(discordgo.ActionsRow)
		for _, comp := range row.Components {
			switch v := comp.(type) {
			case discordgo.Button:
				if cid, ok := parseID(v.CustomID); ok && cid.act == act {
					return found{customID: v.CustomID, disabled: v.Disabled}, true
				}
			case discordgo.SelectMenu:
				if cid, ok := parseID(v.CustomID); ok && cid.act == act {
					f := found{customID: v.CustomID, disabled: v.Disabled}
					for _, o := range v.Options {
						f.options = append(f.options, o.Value)
					}
					return f, true
				}
			}
		}
	}
	return found{}, false
}

func mustFind(t *testing.T, r commands.Reply, act action) found {
	t.Helper()
	f, ok := find(r, act)
	if !ok {
		t.Fatalf("no %q component in reply: %s", act, text(r))
	}
	return f
}

func text(r commands.Reply) string {
	var b strings.Builder
	b.WriteString(r.Content)
	for _, e := range r.Embeds {
		b.WriteString("\n" + e.Title + "\n" + e.Description)
		for _, f := range e.Fields {
			b.WriteString("\n" + f.Name + ": " + f.Value)
		}
		if e.Footer != nil {
			b.WriteString("\n" + e.Footer.Text)
		}
	}
	return b.String()
}

func (e *env) click(t *testing.T, actor auth.Actor, r commands.Reply, act action, values ...string) commands.Reply {
	t.Helper()
	f := mustFind(t, r, act)
	if f.disabled {
		t.Fatalf("%s is disabled in: %s", act, text(r))
	}
	return e.ui.Component(context.Background(), actor, guild, f.customID, values)
}

func (e *env) seed(ids ...string) {
	for _, id := range ids {
		e.ha.put(id, "Name of "+id, "on", nil)
	}
}

// toDiscover starts a session and moves it to the discovery step.
func (e *env) toDiscover(t *testing.T) commands.Reply {
	t.Helper()
	r := e.ui.Start(context.Background(), admin, guild)
	return e.click(t, admin, r, actNext)
}

// assertLimits checks a reply against Discord's documented message limits.
func assertLimits(t *testing.T, name string, r commands.Reply) {
	t.Helper()
	if utf8.RuneCountInString(r.Content) > 2000 {
		t.Errorf("%s: content too long", name)
	}
	if len(r.Components) > maxRows {
		t.Errorf("%s: %d action rows, max %d", name, len(r.Components), maxRows)
	}
	for _, c := range r.Components {
		row, ok := c.(discordgo.ActionsRow)
		if !ok {
			t.Errorf("%s: top-level component %T is not an action row", name, c)
			continue
		}
		if len(row.Components) == 0 || len(row.Components) > maxButtonsInRow {
			t.Errorf("%s: row with %d components", name, len(row.Components))
		}
		for _, comp := range row.Components {
			switch v := comp.(type) {
			case discordgo.Button:
				if utf8.RuneCountInString(v.Label) > maxLabel || len(v.CustomID) > 100 {
					t.Errorf("%s: button %q (%d-char id)", name, v.Label, len(v.CustomID))
				}
				if _, ok := parseID(v.CustomID); !ok {
					t.Errorf("%s: button custom id %q does not parse", name, v.CustomID)
				}
			case discordgo.SelectMenu:
				if len(row.Components) != 1 {
					t.Errorf("%s: a select menu must be alone in its row", name)
				}
				if len(v.Options) == 0 || len(v.Options) > maxSelectOpts || v.MaxValues > maxSelectOpts || len(v.CustomID) > 100 {
					t.Errorf("%s: select with %d options, max %d", name, len(v.Options), v.MaxValues)
				}
				if v.MinValues == nil || v.MaxValues < 1 {
					t.Errorf("%s: select min/max invalid", name)
				}
				for _, o := range v.Options {
					if utf8.RuneCountInString(o.Label) > 100 || utf8.RuneCountInString(o.Description) > 100 || len(o.Value) > 100 || o.Label == "" {
						t.Errorf("%s: option %+v exceeds limits", name, o)
					}
				}
			default:
				t.Errorf("%s: unexpected component %T", name, comp)
			}
		}
	}
	for _, em := range r.Embeds {
		total := utf8.RuneCountInString(em.Title) + utf8.RuneCountInString(em.Description)
		if utf8.RuneCountInString(em.Title) > 256 || utf8.RuneCountInString(em.Description) > 4096 || len(em.Fields) > 25 {
			t.Errorf("%s: embed title/description/fields exceed limits", name)
		}
		for _, f := range em.Fields {
			total += utf8.RuneCountInString(f.Name) + utf8.RuneCountInString(f.Value)
			if utf8.RuneCountInString(f.Name) > 256 || utf8.RuneCountInString(f.Value) > 1024 {
				t.Errorf("%s: field %q exceeds limits", name, f.Name)
			}
		}
		if em.Footer != nil {
			total += utf8.RuneCountInString(em.Footer.Text)
		}
		if total > 6000 {
			t.Errorf("%s: embed has %d characters, max 6000", name, total)
		}
	}
}

func TestParseID_acceptsOnlyWellFormedWhitelistedIDs(t *testing.T) {
	sid := strings.Repeat("ab", sidBytes)
	good := customID{sid: sid, ver: 7, act: actNext}
	bad := map[string]string{
		"empty":                "",
		"wrong prefix":         "xx:" + sid + ":1:next",
		"too few parts":        "nh:" + sid + ":1",
		"too many parts":       "nh:" + sid + ":1:next:extra",
		"short session id":     "nh:abcd:1:next",
		"non hex session id":   "nh:" + strings.Repeat("zz", sidBytes) + ":1:next",
		"zero version":         "nh:" + sid + ":0:next",
		"negative version":     "nh:" + sid + ":-3:next",
		"non numeric version":  "nh:" + sid + ":x:next",
		"unknown action":       "nh:" + sid + ":1:drop_tables",
		"action case":          "nh:" + sid + ":1:NEXT",
		"sql in action":        "nh:" + sid + ":1:next'; DROP TABLE setup_sessions;--",
		"entity id as session": "nh:light.kitchen:1:next",
	}

	parsed, ok := parseID(good.String())

	if !ok || parsed != good {
		t.Errorf("parseID(%q) = %+v, %v", good.String(), parsed, ok)
	}
	for name, raw := range bad {
		if _, ok := parseID(raw); ok {
			t.Errorf("%s: %q was accepted", name, raw)
		}
	}
	if len(good.String()) > 100 {
		t.Errorf("custom id is %d characters, Discord allows 100", len(good.String()))
	}
}

// values returns the option values of a select menu in a reply.
func values(t *testing.T, r commands.Reply, act action, n int) []string {
	t.Helper()
	opts := mustFind(t, r, act).options
	return opts[:min(n, len(opts))]
}

func TestEveryPanel_respectsDiscordLimits_andEscapesHostileHomeAssistantText(t *testing.T) {
	e := newEnv(t)
	for i := range 300 {
		e.ha.put(fmt.Sprintf("light.l%03d", i), fmt.Sprintf("Lamp %03d %s", i, strings.Repeat("very long name ", 12)), "on", map[string]any{"device_class": strings.Repeat("c", 40)})
	}
	e.ha.put("sensor.hostile", "**@everyone** <@123456789> ```code``` [x](http://evil) ~~s~~", "unavailable", nil)
	e.ha.put("sensor.long_id_"+strings.Repeat("x", 90), "Too Long", "1", nil) // ID over 100 characters: cannot be offered
	ctx := context.Background()

	health := e.ui.Start(ctx, admin, guild)
	discover := e.click(t, admin, health, actNext)
	page2 := e.click(t, admin, discover, actPageNext)
	picked := e.click(t, admin, page2, actSelect, values(t, page2, actSelect, 4)...)
	perms := e.click(t, admin, picked, actNext)
	focused := e.click(t, admin, perms, actFocus, mustFind(t, perms, actFocus).options[0])
	controlled := e.click(t, admin, focused, actControl)
	aliases := e.click(t, admin, controlled, actNext)
	focusedAlias := e.click(t, admin, aliases, actFocus, mustFind(t, aliases, actFocus).options[0])
	withAlias := e.ui.Submit(ctx, admin, guild, modalID(t, focusedAlias), map[string]string{"alias": "Reading Lamp"})
	review := e.click(t, admin, withAlias, actNext)
	done := e.click(t, admin, review, actConfirm)

	panels := map[string]commands.Reply{"health": health, "discover": discover, "discover page 2": page2, "after select": picked, "permissions": perms, "permissions focused": focused, "after control": controlled, "aliases": aliases, "aliases focused": focusedAlias, "after alias": withAlias, "review": review, "done": done}
	for name, r := range panels {
		assertLimits(t, name, r)
	}
	hostileFirstPage := text(discover)
	// A backslash neutralizes the character after it, so remove escaped pairs and
	// make sure no active markdown, mention or link syntax is left over.
	active := escapedPair.ReplaceAllString(hostileFirstPage, "")
	for _, raw := range []string{"<@123456789>", "@everyone", "[x](", "~~", "```code", "**@"} {
		if strings.Contains(active, raw) {
			t.Errorf("hostile Home Assistant text %q reached Discord as active syntax:\n%s", raw, hostileFirstPage)
		}
	}
	if !strings.Contains(hostileFirstPage, "\\@everyone") || !strings.Contains(hostileFirstPage, "unavailable (not reporting)") {
		t.Errorf("escaped name or unavailable marker missing:\n%s", hostileFirstPage)
	}
	if !strings.Contains(hostileFirstPage, "1 entities have IDs too long") {
		t.Errorf("entities that cannot be offered must be counted:\n%s", hostileFirstPage)
	}
	if !strings.Contains(text(done), "Setup complete") {
		t.Errorf("done = %s", text(done))
	}
}

// modalID returns the alias modal custom ID that a click on "Set alias" would open.
func modalID(t *testing.T, r commands.Reply) string {
	t.Helper()
	cid, ok := parseID(mustFind(t, r, actAlias).customID)
	if !ok {
		t.Fatal("alias button has no valid id")
	}
	cid.act = actAliasForm
	return cid.String()
}

func TestClicks_staleForgedAndForeign_areRefusedWithoutChangingAnything(t *testing.T) {
	e := newEnv(t)
	e.seed("light.a", "light.b")
	ctx := context.Background()
	first := e.toDiscover(t)
	staleNext := mustFind(t, first, actNext).customID
	second := e.click(t, admin, first, actSelect, "light.a")

	stale := e.ui.Component(ctx, admin, guild, staleNext, nil) // rendered before the selection
	forged := e.click(t, admin, second, actSelect, "sensor.not_there", "light.b")
	garbage := e.ui.Component(ctx, admin, guild, "nh:zzz:1:next", nil)
	foreign := mustFind(t, second, actNext).customID
	otherAdmin := e.ui.Component(ctx, admin2, guild, foreign, nil)
	otherGuild := e.ui.Component(ctx, admin, "g2", foreign, nil)
	notAdmin := e.ui.Component(ctx, regular, guild, foreign, nil)
	after := e.ui.Component(ctx, admin, guild, mustFind(t, second, actRefresh).customID, nil)

	if !strings.Contains(text(stale), "out of date") || !strings.Contains(text(stale), "Selected: **1**") {
		t.Errorf("stale click must refresh, not act:\n%s", text(stale))
	}
	if !strings.Contains(text(forged), "not offered") || !strings.Contains(text(forged), "Selected: **1**") {
		t.Errorf("forged selection must change nothing:\n%s", text(forged))
	}
	if !strings.Contains(text(garbage), "not valid") {
		t.Errorf("garbage id: %s", text(garbage))
	}
	for name, r := range map[string]commands.Reply{"another administrator": otherAdmin, "another guild": otherGuild, "a regular user": notAdmin} {
		if !strings.Contains(r.Content, "not authorized") || len(r.Embeds) != 0 || len(r.Components) != 0 {
			t.Errorf("%s: %+v, want a bare refusal that leaks nothing", name, r)
		}
	}
	if !strings.Contains(text(after), "Selected: **1**") || !strings.Contains(text(after), "Page 1 of 1") {
		t.Errorf("the owner's session was affected:\n%s", text(after))
	}
}

func TestSetup_isAdministratorOnly_atEveryEntryPoint(t *testing.T) {
	e := newEnv(t)
	e.seed("light.a")
	ctx := context.Background()
	discover := e.toDiscover(t)
	search := mustFind(t, discover, actSearch).customID
	form := strings.Replace(search, ":"+string(actSearch), ":"+string(actSearchForm), 1)

	start := e.ui.Start(ctx, regular, guild)
	modal := e.ui.Modal(ctx, regular, guild, search)
	foreignModal := e.ui.Modal(ctx, admin2, guild, search)
	submit := e.ui.Submit(ctx, regular, guild, form, map[string]string{"q": "x"})

	for name, r := range map[string]commands.Reply{"start": start, "modal": modal.Notice, "foreign modal": foreignModal.Notice, "submit": submit} {
		if !strings.Contains(r.Content, "not authorized") || len(r.Components) != 0 {
			t.Errorf("%s: %+v, want a bare refusal", name, r)
		}
	}
	if modal.Modal != nil || foreignModal.Modal != nil {
		t.Error("a modal was opened for someone who may not use setup")
	}
}

func TestModals_openOnlyForTheCurrentPanel_andSubmissionsApplyThroughTheService(t *testing.T) {
	e := newEnv(t)
	e.ha.put("light.kitchen", "Kitchen", "on", nil)
	e.ha.put("light.hall", "Hall", "on", nil)
	ctx := context.Background()
	discover := e.toDiscover(t)
	searchID := mustFind(t, discover, actSearch).customID

	if !e.ui.OpensModal(searchID) || e.ui.OpensModal(mustFind(t, discover, actNext).customID) {
		t.Error("OpensModal must be true only for search and alias")
	}
	res := e.ui.Modal(ctx, admin, guild, searchID)
	submitted := e.ui.Submit(ctx, admin, guild, res.Modal.CustomID, map[string]string{"q": "  KITCHEN  "})
	staleModal := e.ui.Modal(ctx, admin, guild, searchID) // the panel moved on after the search

	if res.Modal == nil || res.Modal.Inputs[0].CustomID != "q" || res.Modal.Inputs[0].MaxLength != 64 {
		t.Fatalf("search modal = %+v", res.Modal)
	}
	if !strings.Contains(text(submitted), "Matches: **1**") || !strings.Contains(text(submitted), "search \"KITCHEN\"") {
		t.Errorf("search results:\n%s", text(submitted))
	}
	if staleModal.Modal != nil || !strings.Contains(staleModal.Notice.Content, "out of date") {
		t.Errorf("stale modal click = %+v", staleModal)
	}

	picked := e.click(t, admin, submitted, actSelect, "light.kitchen")
	perms := e.click(t, admin, picked, actNext)
	aliases := e.click(t, admin, perms, actNext)
	noFocus := e.ui.Modal(ctx, admin, guild, mustFind(t, aliases, actAlias).customID)
	focused := e.click(t, admin, aliases, actFocus, "light.kitchen")
	aliasModalRes := e.ui.Modal(ctx, admin, guild, mustFind(t, focused, actAlias).customID)

	if noFocus.Modal != nil {
		t.Error("the alias modal opened without a focused entity")
	}
	if aliasModalRes.Modal == nil || aliasModalRes.Modal.Inputs[0].MaxLength != entities.MaxAliasRunes || !strings.Contains(aliasModalRes.Modal.Title, "Kitchen") {
		t.Fatalf("alias modal = %+v", aliasModalRes.Modal)
	}
	bad := e.ui.Submit(ctx, admin, guild, aliasModalRes.Modal.CustomID, map[string]string{"alias": "<@everyone>"})
	good := e.ui.Submit(ctx, admin, guild, modalID(t, focused), map[string]string{"alias": "  Main   Kitchen "})
	removed := e.click(t, admin, good, actUnalias)

	if !strings.Contains(text(bad), "not valid") || strings.Contains(text(bad), "Main Kitchen") {
		t.Errorf("invalid alias:\n%s", text(bad))
	}
	if !strings.Contains(text(good), "**Main Kitchen**") {
		t.Errorf("alias not shown normalized:\n%s", text(good))
	}
	if strings.Contains(text(removed), "**Main Kitchen**") {
		t.Errorf("alias not removed:\n%s", text(removed))
	}
}

func TestFullWizard_changesNothingUntilConfirmed_thenAppliesAndReportsExactlyWhatChanged(t *testing.T) {
	e := newEnv(t)
	e.seed("light.kitchen", "switch.fan", "sensor.temp", "light.old")
	ctx := context.Background()
	if err := e.perms.Set(ctx, entities.Grant{ID: "light.old", Access: entities.AccessControl, GrantedBy: "a1", At: *e.clock}); err != nil {
		t.Fatal(err)
	}
	discover := e.toDiscover(t)
	picked := e.click(t, admin, discover, actSelect, "light.kitchen", "switch.fan", "sensor.temp", "light.old")
	perms := e.click(t, admin, picked, actNext)
	r := e.click(t, admin, perms, actBulkControl) // lights/switches control, sensor read
	r = e.click(t, admin, r, actFocus, "light.old")
	r = e.click(t, admin, r, actNone) // revoke the old light
	aliases := e.click(t, admin, r, actNext)
	r = e.click(t, admin, aliases, actFocus, "light.kitchen")
	r = e.ui.Submit(ctx, admin, guild, modalID(t, r), map[string]string{"alias": "Kitchen"})
	review := e.click(t, admin, r, actNext)

	beforeConfirm, _ := e.perms.List(ctx)
	confirm := mustFind(t, review, actConfirm)
	reviewText := text(review)
	done := e.click(t, admin, review, actConfirm)
	afterConfirm, _ := e.perms.List(ctx)
	replay := e.ui.Component(ctx, admin, guild, confirm.customID, nil)

	if len(beforeConfirm) != 1 {
		t.Errorf("nothing may be written before confirmation: %+v", beforeConfirm)
	}
	for _, want := range []string{"Newly shared (3)", "Access revoked (1)", "will lose access", "Alias changes (1)", "light.old"} {
		if !strings.Contains(reviewText, want) {
			t.Errorf("review lacks %q:\n%s", want, reviewText)
		}
	}
	if confirm.disabled || !strings.Contains(text(done), "3 newly shared, 0 modified, 1 revoked, 1 alias changes") {
		t.Errorf("done = %s", text(done))
	}
	got := map[entities.ID]entities.Access{}
	for _, p := range afterConfirm {
		got[p.ID] = p.Access
	}
	if len(got) != 3 || got["light.kitchen"] != entities.AccessControl || got["switch.fan"] != entities.AccessControl || got["sensor.temp"] != entities.AccessRead {
		t.Errorf("permissions after confirm = %+v", got)
	}
	if !strings.Contains(text(replay), "already applied") || len(replay.Components) != 0 {
		t.Errorf("a replayed confirmation must be a harmless notice: %s", text(replay))
	}
	if alias, _ := e.aliases.Get(ctx, "light.kitchen"); alias != "Kitchen" {
		t.Errorf("alias = %q", alias)
	}
}

func TestReview_blocksConfirmationWhenThereIsNothingToApply(t *testing.T) {
	e := newEnv(t)
	e.seed("light.a")
	if err := e.perms.Set(context.Background(), entities.Grant{ID: "light.a", Access: entities.AccessControl, GrantedBy: "a1", At: *e.clock}); err != nil {
		t.Fatal(err)
	}
	discover := e.toDiscover(t)
	r := e.click(t, admin, discover, actSelect, "light.a") // already shared: nothing changes
	r = e.click(t, admin, r, actNext)
	r = e.click(t, admin, r, actNext)
	review := e.click(t, admin, r, actNext)

	if !mustFind(t, review, actConfirm).disabled || !strings.Contains(text(review), "Nothing to apply") {
		t.Errorf("review = %s", text(review))
	}
}

func TestResume_offersContinueOrStartOver_andKeepsProgress(t *testing.T) {
	e := newEnv(t)
	e.seed("light.a", "light.b")
	ctx := context.Background()
	discover := e.toDiscover(t)
	selected := e.click(t, admin, discover, actSelect, "light.a")

	prompt := e.ui.Start(ctx, admin, guild)
	resumed := e.click(t, admin, prompt, actResume)
	restarted := e.click(t, admin, e.ui.Start(ctx, admin, guild), actRestart)
	oldID, _ := parseID(mustFind(t, selected, actNext).customID)
	old, oldErr := e.svc.Get(ctx, setup.Caller{Actor: admin, GuildID: guild}, oldID.sid)

	if !strings.Contains(text(prompt), "step 2 of 6") || !strings.Contains(text(prompt), "**1** entities selected") {
		t.Errorf("resume prompt = %s", text(prompt))
	}
	if !strings.Contains(text(resumed), "Selected: **1**") || !strings.Contains(text(resumed), "Discover devices") {
		t.Errorf("resumed panel lost progress:\n%s", text(resumed))
	}
	if !strings.Contains(text(restarted), "Connection health") || oldErr != nil || old.Status != setup.StatusCancelled {
		t.Errorf("restart: %s old=%v err=%v", text(restarted), old.Status, oldErr)
	}
}

func TestCancelAndExpiry_endTheSessionWithoutComponents(t *testing.T) {
	e := newEnv(t)
	e.seed("light.a")
	ctx := context.Background()
	discover := e.toDiscover(t)
	expiring := mustFind(t, discover, actRefresh).customID

	cancelled := e.click(t, admin, discover, actCancel)
	afterCancel := e.ui.Component(ctx, admin, guild, expiring, nil)
	fresh := e.toDiscover(t)
	*e.clock = e.clock.Add(setup.SessionTTL + time.Minute)
	expired := e.ui.Component(ctx, admin, guild, mustFind(t, fresh, actRefresh).customID, nil)

	for name, r := range map[string]commands.Reply{"cancelled": cancelled, "click after cancel": afterCancel, "expired": expired} {
		if len(r.Components) != 0 || len(r.Embeds) != 1 {
			t.Errorf("%s must end with no components: %+v", name, r)
		}
	}
	if !strings.Contains(text(cancelled), "Nothing was changed") || !strings.Contains(text(expired), "expired") {
		t.Errorf("cancelled=%s expired=%s", text(cancelled), text(expired))
	}
}

func TestHomeAssistantDown_blocksProgress_andRecoversOnRetry(t *testing.T) {
	e := newEnv(t)
	e.seed("light.a")
	ctx := context.Background()
	e.health.rep.HAErr = fmt.Errorf("%w: connection refused", rest.ErrUnavailable)
	health := e.ui.Start(ctx, admin, guild)

	blocked := mustFind(t, health, actNext).disabled
	e.health.rep.HAErr = nil
	e.ha.err = errors.New("connection refused")
	refusedNext := e.ui.Component(ctx, admin, guild, mustFind(t, e.ui.Start(ctx, admin, guild), actNext).customID, nil)
	e.ha.err = nil
	discover := e.click(t, admin, e.ui.Start(ctx, admin, guild), actNext)
	e.ha.err = errors.New("connection refused")
	*e.clock = e.clock.Add(entities.SnapshotTTL + time.Second)
	down := e.click(t, admin, discover, actRefresh)
	e.ha.err = nil
	recovered := e.click(t, admin, down, actRetry)

	if !blocked || !strings.Contains(text(health), "Unreachable") || !strings.Contains(text(health), "never uses stale") {
		t.Errorf("health with Home Assistant down: blocked=%v\n%s", blocked, text(health))
	}
	if !strings.Contains(text(refusedNext), "Home Assistant is not reachable") {
		t.Errorf("Next with Home Assistant down must not progress:\n%s", text(refusedNext))
	}
	if !strings.Contains(text(down), "not reachable") || len(down.Components) != 1 {
		t.Errorf("unavailable panel = %s", text(down))
	}
	if !strings.Contains(text(recovered), "Discover devices") || !strings.Contains(text(recovered), "Discovered: **1**") {
		t.Errorf("recovery:\n%s", text(recovered))
	}
}

func TestDiscovery_pagesFiltersAndSharedView(t *testing.T) {
	e := newEnv(t)
	for i := range 25 {
		e.ha.put(fmt.Sprintf("light.l%02d", i), fmt.Sprintf("Lamp %02d", i), "off", nil)
	}
	e.ha.put("switch.fan", "Fan", "off", nil)
	if err := e.perms.Set(context.Background(), entities.Grant{ID: "switch.fan", Access: entities.AccessRead, GrantedBy: "a1", At: *e.clock}); err != nil {
		t.Fatal(err)
	}
	discover := e.toDiscover(t)
	prev := mustFind(t, discover, actPagePrev).disabled
	page2 := e.click(t, admin, discover, actPageNext)
	page3 := e.click(t, admin, page2, actPageNext)
	lastNext := mustFind(t, page3, actPageNext).disabled
	lightsOnly := e.click(t, admin, page3, actDomain, "light")
	sharedOnly := e.click(t, admin, lightsOnly, actDomain, domainShared)
	forged := e.click(t, admin, sharedOnly, actDomain, "light; DROP TABLE")
	all := e.click(t, admin, forged, actDomain, domainAll)

	if !prev || !strings.Contains(text(page2), "Page 2 of 3") || !strings.Contains(text(page3), "Page 3 of 3") || !lastNext {
		t.Errorf("paging: prev disabled=%v lastNext disabled=%v\n%s", prev, lastNext, text(page3))
	}
	if !strings.Contains(text(lightsOnly), "Matches: **25**") || !strings.Contains(text(lightsOnly), "domain `light`") || !strings.Contains(text(lightsOnly), "Page 1 of 3") {
		t.Errorf("domain filter must reset to page 1:\n%s", text(lightsOnly))
	}
	if !strings.Contains(text(sharedOnly), "Matches: **1**") || !strings.Contains(text(sharedOnly), "switch.fan") || !strings.Contains(text(sharedOnly), "read") {
		t.Errorf("shared view:\n%s", text(sharedOnly))
	}
	if !strings.Contains(text(forged), "out of date") || !strings.Contains(text(forged), "Matches: **1**") {
		t.Errorf("a forged domain value must change nothing:\n%s", text(forged))
	}
	if !strings.Contains(text(all), "Matches: **26**") {
		t.Errorf("all:\n%s", text(all))
	}
}
