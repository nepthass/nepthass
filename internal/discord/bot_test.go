package discord

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/config"
	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/homeassistant/rest"
	"github.com/nepthass/nepthass/internal/homeassistant/ws"
	"github.com/nepthass/nepthass/internal/monitor"
	"github.com/nepthass/nepthass/internal/setup"
	"github.com/nepthass/nepthass/internal/storage"
	"github.com/nepthass/nepthass/tests/fakeha"
)

const (
	guildID    = "500000000000000001"
	adminID    = "500000000000000002"
	userID     = "500000000000000003"
	outsider   = "500000000000000004"
	otherGuild = "500000000000000099"
	haToken    = "ha-token-for-tests"
)

type fakeAPI struct {
	mu        sync.Mutex
	responses []*discordgo.InteractionResponse
	edits     []*discordgo.WebhookEdit
}

func (f *fakeAPI) InteractionRespond(_ *discordgo.Interaction, resp *discordgo.InteractionResponse, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses = append(f.responses, resp)
	return nil
}

func (f *fakeAPI) InteractionResponseEdit(_ *discordgo.Interaction, edit *discordgo.WebhookEdit, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, edit)
	return &discordgo.Message{}, nil
}

// reset forgets what was sent so far.
func (f *fakeAPI) reset() { f.responses, f.edits = nil, nil }

// finalText is the text the user finally sees: the edited reply of a deferred
// interaction, or the immediate message.
func (f *fakeAPI) finalText() string {
	var b strings.Builder
	if n := len(f.edits); n > 0 {
		e := f.edits[n-1]
		b.WriteString(*e.Content)
		for _, emb := range *e.Embeds {
			b.WriteString("\n" + emb.Title + "\n" + emb.Description)
			for _, f := range emb.Fields {
				b.WriteString("\n" + f.Name + ": " + f.Value)
			}
			if emb.Footer != nil {
				b.WriteString("\n" + emb.Footer.Text)
			}
		}
		return b.String()
	}
	if n := len(f.responses); n > 0 && f.responses[n-1].Data != nil {
		return f.responses[n-1].Data.Content
	}
	return ""
}

type harness struct {
	bot   *Bot
	api   *fakeAPI
	ha    *fakeha.Server
	perms *entities.Permissions
	audit *audit.Log
	mon   *monitor.Monitor
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ha := fakeha.New(t, haToken)
	client := rest.New(config.HomeAssistant{URL: ha.URL, Token: haToken, Timeout: 2 * time.Second})
	logger := slog.New(slog.DiscardHandler)
	auditLog := audit.New(db)
	perms := entities.NewPermissions(db)
	disc := entities.NewDiscovery(entities.DiscoveryDeps{HA: client, DB: db, Permissions: perms, Now: time.Now})
	setupSvc := setup.NewService(setup.Deps{DB: db, Store: setup.NewStore(db), Discovery: disc, Permissions: perms, Aliases: entities.NewAliases(db), Audit: auditLog, Now: time.Now})
	svc := entities.NewService(entities.Deps{HA: client, Permissions: perms, Aliases: entities.NewAliases(db), Audit: auditLog, Logger: logger, Now: time.Now})
	aliases := entities.NewAliases(db)
	mon, err := monitor.New(monitor.Config{
		HomeAssistant: config.HomeAssistant{URL: ha.URL, Token: haToken, Timeout: 2 * time.Second},
		Backoff:       monitor.Backoff{Initial: 2 * time.Millisecond, Max: 16 * time.Millisecond, AuthInitial: 20 * time.Millisecond, AuthMax: 80 * time.Millisecond, StableAfter: time.Hour},
		Session:       ws.Options{HandshakeTimeout: time.Second, PingInterval: time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	bot, err := New(config.Discord{
		Token: "discord-token-for-tests", GuildID: guildID, AdminUserIDs: []string{adminID}, AllowedUserIDs: []string{userID},
	}, commands.Deps{Entities: svc, Discovery: disc, Monitoring: monitor.NewReader(mon, perms, aliases), Audit: auditLog, HA: client, DB: db, Logger: logger, Version: "test"}, setupSvc)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{bot: bot, api: &fakeAPI{}, ha: ha, perms: perms, audit: auditLog, mon: mon}
}

func (h *harness) send(i *discordgo.Interaction) {
	h.api.reset()
	h.bot.serve(context.Background(), h.api, i)
}

func str(name, value string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionString, Value: value}
}

func sub(name string, opts ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionSubCommand, Options: opts}
}

func slash(guild, user, name string, opts ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.Interaction {
	return &discordgo.Interaction{
		Type: discordgo.InteractionApplicationCommand, GuildID: guild,
		Member: &discordgo.Member{User: &discordgo.User{ID: user}},
		Data:   discordgo.ApplicationCommandInteractionData{Name: name, Options: opts},
	}
}

func assertEphemeral(t *testing.T, api *fakeAPI) {
	t.Helper()
	if len(api.responses) == 0 {
		t.Fatal("no response sent")
	}
	for _, r := range api.responses {
		if r.Data == nil || r.Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
			t.Errorf("response is not ephemeral: %+v", r)
		}
	}
}

func TestInteraction_rejectsUnauthorizedUsers_beforeAnyCommandRuns(t *testing.T) {
	h := newHarness(t)
	h.ha.Set("light.kitchen", "off", nil)
	if err := h.perms.Set(context.Background(), entities.Grant{ID: "light.kitchen", Access: entities.AccessControl, GrantedBy: adminID, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	control := []*discordgo.ApplicationCommandInteractionDataOption{str("entity", "light.kitchen"), str("action", "on")}
	attempts := map[string]*discordgo.Interaction{
		"user not on any allowlist":    slash(guildID, outsider, "control", control...),
		"admin id used in wrong guild": slash(otherGuild, adminID, "control", control...),
		"admin id in a direct message": {Type: discordgo.InteractionApplicationCommand, User: &discordgo.User{ID: adminID}, Data: discordgo.ApplicationCommandInteractionData{Name: "control", Options: control}},
		"interaction without any user": {Type: discordgo.InteractionApplicationCommand, GuildID: guildID, Data: discordgo.ApplicationCommandInteractionData{Name: "ping"}},
	}
	for name, i := range attempts {
		t.Run(name, func(t *testing.T) {
			h.send(i)

			if got := h.api.finalText(); got != "You are not authorized to use NeptHass." {
				t.Errorf("reply = %q", got)
			}
			assertEphemeral(t, h.api)
			if len(h.api.edits) != 0 {
				t.Error("an unauthorized interaction was deferred or answered by a command")
			}
		})
	}
	if calls := h.ha.Calls(); len(calls) != 0 {
		t.Errorf("Home Assistant received %+v", calls)
	}
}

func TestInteraction_deferredReplyIsEphemeral_andDisablesMentions(t *testing.T) {
	h := newHarness(t)

	h.send(slash(guildID, userID, "ping"))

	assertEphemeral(t, h.api)
	if len(h.api.responses) != 1 || h.api.responses[0].Type != discordgo.InteractionResponseDeferredChannelMessageWithSource {
		t.Fatalf("responses = %+v", h.api.responses)
	}
	if len(h.api.edits) != 1 || !strings.HasPrefix(*h.api.edits[0].Content, "Pong!") {
		t.Fatalf("edits = %+v", h.api.edits)
	}
	if m := h.api.edits[0].AllowedMentions; m == nil || m.Parse == nil || len(m.Parse) != 0 {
		t.Errorf("allowed mentions = %+v, want an explicit empty parse list", m)
	}
}

func TestInteraction_adminSharesDevice_thenUserControlsIt_whileOutsiderCannot(t *testing.T) {
	h := newHarness(t)
	h.ha.Set("light.kitchen", "off", map[string]any{"friendly_name": "Kitchen"})

	h.send(slash(guildID, adminID, "permissions", sub("allow", str("entity", "light.kitchen"), str("access", "control"))))
	allowText := h.api.finalText()
	h.send(slash(guildID, userID, "control", str("entity", "light.kitchen"), str("action", "on")))
	controlText := h.api.finalText()
	callsAfterUser := len(h.ha.Calls())
	h.send(slash(guildID, outsider, "control", str("entity", "light.kitchen"), str("action", "off")))

	if !strings.Contains(allowText, "now has **control** access") {
		t.Errorf("allow reply = %q", allowText)
	}
	if !strings.Contains(controlText, "Command Sent") || !strings.Contains(controlText, "off \u2192 on") {
		t.Errorf("control reply = %q", controlText)
	}
	if callsAfterUser != 1 || len(h.ha.Calls()) != 1 {
		t.Errorf("service calls = %+v", h.ha.Calls())
	}
	entries, err := h.audit.Recent(context.Background(), 10)
	if err != nil || len(entries) != 2 || entries[0].Command != "control" || entries[0].ActorID != userID || entries[1].Command != "permissions.allow" {
		t.Errorf("audit = %+v err=%v", entries, err)
	}
}

func TestInteraction_regularUserCannotUseAdministratorCommands(t *testing.T) {
	h := newHarness(t)
	h.ha.Set("light.kitchen", "off", nil)

	h.send(slash(guildID, userID, "permissions", sub("allow", str("entity", "light.kitchen"), str("access", "control"))))

	if got := h.api.finalText(); !strings.Contains(got, "administrators") {
		t.Errorf("reply = %q", got)
	}
	if perms, _ := h.perms.List(context.Background()); len(perms) != 0 {
		t.Errorf("permissions = %+v", perms)
	}
}

func TestInteraction_rateLimitsABurstOfCommands(t *testing.T) {
	h := newHarness(t)
	fixed := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	h.bot.limiter = auth.NewLimiter(2, time.Minute, func() time.Time { return fixed })

	h.send(slash(guildID, userID, "ping"))
	h.send(slash(guildID, userID, "ping"))
	h.send(slash(guildID, userID, "ping"))

	if got := h.api.finalText(); !strings.Contains(got, "too quickly") {
		t.Errorf("third reply = %q", got)
	}
	h.send(slash(guildID, adminID, "ping"))
	if got := h.api.finalText(); !strings.HasPrefix(got, "Pong!") {
		t.Errorf("another user was affected by the limit: %q", got)
	}
}

func TestInteraction_componentsAreAuthorizedLikeCommands(t *testing.T) {
	h := newHarness(t)
	component := func(user string) *discordgo.Interaction {
		return &discordgo.Interaction{
			Type: discordgo.InteractionMessageComponent, GuildID: guildID,
			Member: &discordgo.Member{User: &discordgo.User{ID: user}},
			Data:   discordgo.MessageComponentInteractionData{CustomID: "anything"},
		}
	}

	h.send(component(outsider))
	outsiderText := h.api.finalText()
	h.send(component(userID))
	userText := h.api.finalText()

	if outsiderText != "You are not authorized to use NeptHass." {
		t.Errorf("outsider reply = %q", outsiderText)
	}
	if !strings.Contains(userText, "not supported") {
		t.Errorf("user reply = %q", userText)
	}
}

func TestInteraction_autocompleteIsAuthorizedAndScoped(t *testing.T) {
	h := newHarness(t)
	if err := h.perms.Set(context.Background(), entities.Grant{ID: "light.kitchen", Access: entities.AccessControl, GrantedBy: adminID, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	complete := func(user string) *discordgo.Interaction {
		return &discordgo.Interaction{
			Type: discordgo.InteractionApplicationCommandAutocomplete, GuildID: guildID,
			Member: &discordgo.Member{User: &discordgo.User{ID: user}},
			Data: discordgo.ApplicationCommandInteractionData{Name: "control", Options: []*discordgo.ApplicationCommandInteractionDataOption{
				{Name: "entity", Type: discordgo.ApplicationCommandOptionString, Value: "kit", Focused: true},
			}},
		}
	}

	h.send(complete(userID))
	allowed := h.api.responses[0]
	h.send(complete(outsider))
	denied := h.api.responses[0]

	if allowed.Type != discordgo.InteractionApplicationCommandAutocompleteResult || len(allowed.Data.Choices) != 1 || allowed.Data.Choices[0].Value != "light.kitchen" {
		t.Errorf("authorized choices = %+v", allowed.Data)
	}
	if denied.Type != discordgo.InteractionApplicationCommandAutocompleteResult || len(denied.Data.Choices) != 0 || denied.Data.Choices == nil {
		t.Errorf("unauthorized choices = %+v", denied.Data)
	}
}

func TestServe_survivesMalformedInteractions(t *testing.T) {
	h := newHarness(t)
	malformed := &discordgo.Interaction{
		Type: discordgo.InteractionApplicationCommand, GuildID: guildID,
		Member: &discordgo.Member{User: &discordgo.User{ID: userID}}, Data: nil,
	}

	h.send(malformed) // must not panic
}

func TestNewRequest_flattensSubcommandsAndIntegers(t *testing.T) {
	data := discordgo.ApplicationCommandInteractionData{Name: "permissions", Options: []*discordgo.ApplicationCommandInteractionDataOption{
		sub("allow", str("entity", "light.x"), &discordgo.ApplicationCommandInteractionDataOption{
			Name: "page", Type: discordgo.ApplicationCommandOptionInteger, Value: float64(3), Focused: true,
		}),
	}}

	req := newRequest(auth.Actor{UserID: userID, Level: auth.LevelUser}, guildID, data)

	if req.Command != "permissions" || req.Subcommand != "allow" || req.Args["entity"] != "light.x" || req.Args["page"] != "3" || req.Focused != "page" {
		t.Errorf("request = %+v", req)
	}
}

// --- v0.2.0: setup wizard and /sync through the real interaction pipeline ---

func componentClick(guild, user, customID string, values ...string) *discordgo.Interaction {
	return &discordgo.Interaction{
		Type: discordgo.InteractionMessageComponent, GuildID: guild,
		Member: &discordgo.Member{User: &discordgo.User{ID: user}},
		Data:   discordgo.MessageComponentInteractionData{CustomID: customID, ComponentType: discordgo.ButtonComponent, Values: values},
	}
}

func modalSubmit(guild, user, customID string, fields map[string]string) *discordgo.Interaction {
	var rows []discordgo.MessageComponent
	for k, v := range fields {
		rows = append(rows, &discordgo.ActionsRow{Components: []discordgo.MessageComponent{&discordgo.TextInput{CustomID: k, Value: v}}})
	}
	return &discordgo.Interaction{
		Type: discordgo.InteractionModalSubmit, GuildID: guild,
		Member: &discordgo.Member{User: &discordgo.User{ID: user}},
		Data:   discordgo.ModalSubmitInteractionData{CustomID: customID, Components: rows},
	}
}

// customIDs returns the custom IDs of the last edited message, keyed by action.
func (f *fakeAPI) customIDs(t *testing.T) map[string]string {
	t.Helper()
	if len(f.edits) == 0 || f.edits[len(f.edits)-1].Components == nil {
		t.Fatalf("no edited message with components; edits=%d", len(f.edits))
	}
	ids := map[string]string{}
	for _, c := range *f.edits[len(f.edits)-1].Components {
		row, _ := c.(discordgo.ActionsRow)
		for _, comp := range row.Components {
			var id string
			switch v := comp.(type) {
			case discordgo.Button:
				id = v.CustomID
			case discordgo.SelectMenu:
				id = v.CustomID
			}
			if parts := strings.Split(id, ":"); len(parts) == 4 {
				ids[parts[3]] = id
			}
		}
	}
	return ids
}

func (h *harness) click(t *testing.T, user, action string, values ...string) {
	t.Helper()
	id, ok := h.api.customIDs(t)[action]
	if !ok {
		t.Fatalf("no %q component in the last message: %s", action, h.api.finalText())
	}
	h.send(componentClick(guildID, user, id, values...))
}

func TestSetup_runsEndToEndThroughTheBot_andAppliesOnlyAfterConfirmation(t *testing.T) {
	h := newHarness(t)
	h.ha.Set("light.kitchen", "off", map[string]any{"friendly_name": "Kitchen"})
	h.ha.Set("sensor.temp", "21.5", map[string]any{"friendly_name": "Temperature", "unit_of_measurement": "\u00b0C", "device_class": "temperature"})
	ctx := context.Background()

	h.send(slash(guildID, adminID, "setup"))
	assertEphemeral(t, h.api)
	if !strings.Contains(h.api.finalText(), "Connection health") || h.api.responses[0].Type != discordgo.InteractionResponseDeferredChannelMessageWithSource {
		t.Fatalf("/setup = %q (%+v)", h.api.finalText(), h.api.responses)
	}
	h.click(t, adminID, "next")
	if h.api.responses[0].Type != discordgo.InteractionResponseDeferredMessageUpdate {
		t.Errorf("a component click must be acknowledged with a deferred update, got %v", h.api.responses[0].Type)
	}
	h.click(t, adminID, "sel", "light.kitchen", "sensor.temp")
	h.click(t, adminID, "next") // permissions
	h.click(t, adminID, "focus", "light.kitchen")
	h.click(t, adminID, "ctrl")
	h.click(t, adminID, "next") // aliases
	h.click(t, adminID, "focus", "light.kitchen")
	h.click(t, adminID, "alias") // opens a modal immediately
	modal := h.api.responses[0]
	if modal.Type != discordgo.InteractionResponseModal || modal.Data == nil || len(modal.Data.Components) != 1 || !strings.HasSuffix(modal.Data.CustomID, ":aliasm") {
		t.Fatalf("alias click = %+v, want an immediate modal response", modal)
	}
	h.send(modalSubmit(guildID, adminID, modal.Data.CustomID, map[string]string{"alias": "Kitchen Light"}))
	h.click(t, adminID, "next") // review
	reviewText := h.api.finalText()
	before, _ := h.perms.List(ctx)
	h.click(t, adminID, "confirm")

	after, _ := h.perms.List(ctx)
	if len(before) != 0 || len(after) != 2 || !strings.Contains(h.api.finalText(), "Setup complete") {
		t.Errorf("before=%d after=%d final=%q", len(before), len(after), h.api.finalText())
	}
	if !strings.Contains(reviewText, "Newly shared (2)") || !strings.Contains(reviewText, "Kitchen Light") {
		t.Errorf("review = %s", reviewText)
	}
	// Immediately usable, by alias, without a restart.
	h.send(slash(guildID, adminID, "control", str("entity", "kitchen light"), str("action", "on")))
	if !strings.Contains(h.api.finalText(), "Command Sent") || len(h.ha.Calls()) != 1 || h.ha.Calls()[0].EntityID != "light.kitchen" {
		t.Errorf("control by alias after setup: %q calls=%+v", h.api.finalText(), h.ha.Calls())
	}
	edit := h.api.edits[len(h.api.edits)-1]
	if m := edit.AllowedMentions; m == nil || m.Parse == nil || len(m.Parse) != 0 {
		t.Errorf("mentions must be disabled on every edit: %+v", m)
	}
}

func TestSetup_isRefusedForEveryoneButAdministrators_andComponentsCannotBeReplayedByOthers(t *testing.T) {
	h := newHarness(t)
	h.ha.Set("light.kitchen", "off", nil)
	h.send(slash(guildID, adminID, "setup"))
	h.click(t, adminID, "next")
	adminIDs := h.api.customIDs(t)

	h.send(slash(guildID, userID, "setup")) // a regular allowed user
	userSetup := h.api.finalText()
	h.send(componentClick(guildID, userID, adminIDs["refresh"]))
	userClick := h.api.finalText()
	h.send(componentClick(guildID, outsider, adminIDs["refresh"]))
	outsiderClick := h.api.finalText()
	h.send(componentClick(otherGuild, adminID, adminIDs["refresh"]))
	wrongGuildClick := h.api.finalText()
	h.send(modalSubmit(guildID, userID, strings.Replace(adminIDs["refresh"], ":refresh", ":searchm", 1), map[string]string{"q": "x"}))
	userSubmit := h.api.finalText()

	if !strings.Contains(userSetup, "administrators") {
		t.Errorf("/setup by a regular user = %q", userSetup)
	}
	if !strings.Contains(userClick, "not authorized to use NeptHass setup") || !strings.Contains(userSubmit, "not authorized to use NeptHass setup") {
		t.Errorf("a regular user replayed an administrator's component: %q / %q", userClick, userSubmit)
	}
	for name, got := range map[string]string{"outsider": outsiderClick, "wrong guild": wrongGuildClick} {
		if got != "You are not authorized to use NeptHass." {
			t.Errorf("%s: %q", name, got)
		}
	}
	h.send(componentClick(guildID, adminID, adminIDs["refresh"]))
	if !strings.Contains(h.api.finalText(), "Discover devices") {
		t.Errorf("the owner's session was affected: %q", h.api.finalText())
	}
}

func TestSetup_aReplayedConfirmationNeverAppliesTwice(t *testing.T) {
	h := newHarness(t)
	h.ha.Set("light.kitchen", "off", nil)
	h.send(slash(guildID, adminID, "setup"))
	h.click(t, adminID, "next")
	h.click(t, adminID, "sel", "light.kitchen")
	h.click(t, adminID, "next")
	h.click(t, adminID, "next")
	h.click(t, adminID, "next")
	confirm := h.api.customIDs(t)["confirm"]

	h.send(componentClick(guildID, adminID, confirm))
	first := h.api.finalText()
	if err := h.perms.Set(context.Background(), entities.Grant{ID: "light.kitchen", Access: entities.AccessRead, GrantedBy: adminID, At: time.Now()}); err != nil {
		t.Fatal(err) // someone changes the permission afterwards
	}
	h.send(componentClick(guildID, adminID, confirm))
	replay := h.api.finalText()

	access, _ := h.perms.Get(context.Background(), "light.kitchen")
	entries, _ := h.audit.Recent(context.Background(), 20)
	applies := 0
	for _, e := range entries {
		if e.Command == "setup.apply" {
			applies++
		}
	}
	if !strings.Contains(first, "Setup complete") || !strings.Contains(replay, "already applied") {
		t.Errorf("first=%q replay=%q", first, replay)
	}
	if access != entities.AccessRead || applies != 1 {
		t.Errorf("the replay overwrote a later change or audited twice: access=%q applies=%d", access, applies)
	}
}

func TestComponents_haveTheirOwnRateLimit_andOnlyWizardIDsAreAccepted(t *testing.T) {
	h := newHarness(t)
	h.ha.Set("light.kitchen", "off", nil)
	h.send(slash(guildID, adminID, "setup"))
	refresh := h.api.customIDs(t)["retry"]
	fixed := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	h.bot.componentLimiter = auth.NewLimiter(3, time.Minute, func() time.Time { return fixed })

	for range 3 {
		h.send(componentClick(guildID, adminID, refresh))
	}
	h.send(componentClick(guildID, adminID, refresh))
	limited := h.api.finalText()
	h.send(componentClick(guildID, userID, "someone-elses-button"))

	if !strings.Contains(limited, "too quickly") {
		t.Errorf("fourth click = %q", limited)
	}
	if got := h.api.finalText(); !strings.Contains(got, "not supported") && !strings.Contains(got, "too quickly") {
		t.Errorf("a foreign component id was not rejected: %q", got)
	}
}

func TestSync_reportsThroughTheBot_forAdministratorsOnly_andNeverShares(t *testing.T) {
	h := newHarness(t)
	h.ha.Set("light.a", "on", nil)
	h.ha.Set("sensor.t", "1", nil)

	h.send(slash(guildID, adminID, "sync"))
	first := h.api.finalText()
	h.ha.Set("light.new", "off", nil)
	h.send(slash(guildID, adminID, "sync"))
	second := h.api.finalText()
	h.send(slash(guildID, userID, "sync"))
	denied := h.api.finalText()

	if !strings.Contains(first, "first synchronization") || !strings.Contains(first, "**2** entities") {
		t.Errorf("first sync = %q", first)
	}
	if !strings.Contains(second, "New since the last sync (1)") || !strings.Contains(second, "light.new") || !strings.Contains(second, "Nothing was shared or changed") {
		t.Errorf("second sync = %q", second)
	}
	if !strings.Contains(denied, "administrators") {
		t.Errorf("/sync by a regular user = %q", denied)
	}
	if perms, _ := h.perms.List(context.Background()); len(perms) != 0 {
		t.Errorf("sync shared something: %+v", perms)
	}
}

// --- v0.3.0: live monitoring through the real interaction pipeline ---

// startMonitoring runs the monitor against the fake Home Assistant and waits until it is healthy.
func (h *harness) startMonitoring(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go h.mon.Run(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.mon.Done():
		case <-time.After(10 * time.Second):
			t.Error("the monitor did not stop")
		}
	})
	h.waitMonitor(t, func(s monitor.Status) bool { return s.Healthy })
}

func (h *harness) waitMonitor(t *testing.T, pred func(monitor.Status) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.mon.WaitFor(ctx, pred); err != nil {
		t.Fatalf("monitoring condition not reached: %v (%+v)", err, h.mon.Status())
	}
}

func TestSensors_showLiveChangesThroughTheBot_andRevocationAppliesImmediately(t *testing.T) {
	h := newHarness(t)
	h.ha.Set("binary_sensor.door", "off", map[string]any{"device_class": "door", "friendly_name": "Front Door"})
	h.ha.Set("binary_sensor.leak", "on", map[string]any{"device_class": "moisture", "friendly_name": "Basement Leak"})
	if err := h.perms.Set(context.Background(), entities.Grant{ID: "binary_sensor.door", Access: entities.AccessRead, GrantedBy: adminID, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	h.startMonitoring(t)

	h.send(slash(guildID, userID, "sensors"))
	closed := h.api.finalText()
	h.ha.Update("binary_sensor.door", "on", map[string]any{"device_class": "door", "friendly_name": "Front Door"})
	h.waitMonitor(t, func(monitor.Status) bool { s, _ := h.mon.Tracker().Get("binary_sensor.door"); return s.Raw == "on" })
	h.send(slash(guildID, userID, "sensors"))
	opened := h.api.finalText()
	h.send(slash(guildID, userID, "health"))
	health := h.api.finalText()
	if _, err := h.perms.Revoke(context.Background(), "binary_sensor.door"); err != nil {
		t.Fatal(err)
	}
	h.send(slash(guildID, userID, "sensors"))
	revoked := h.api.finalText()

	if !strings.Contains(closed, "closed (off)") || !strings.Contains(opened, "open (on)") || strings.Contains(opened, "last known values") {
		t.Errorf("closed:\n%s\nopened:\n%s", closed, opened)
	}
	if strings.Contains(closed+opened+health+revoked, "Basement") || strings.Contains(closed+opened, "binary_sensor.leak") {
		t.Error("an unshared sensor leaked through the bot")
	}
	if !strings.Contains(health, "Healthy: 2 sensors tracked") || !strings.Contains(health, "Snapshot ✅") {
		t.Errorf("health = %s", health)
	}
	if strings.Contains(revoked, "Front Door") || !strings.Contains(revoked, "No monitored sensors are shared") {
		t.Errorf("after revocation:\n%s", revoked)
	}
}

func TestSensors_aStrangerGetsNothing_andHealthNeverShowsInfrastructure(t *testing.T) {
	h := newHarness(t)
	h.ha.Set("binary_sensor.door", "on", map[string]any{"device_class": "door"})
	h.startMonitoring(t)

	h.send(slash(guildID, outsider, "sensors"))
	stranger := h.api.finalText()
	h.send(slash(guildID, userID, "health"))
	health := h.api.finalText()

	if stranger != "You are not authorized to use NeptHass." {
		t.Errorf("stranger = %q", stranger)
	}
	for _, forbidden := range []string{"127.0.0.1", "http://", "ws://", haToken} {
		if strings.Contains(health, forbidden) {
			t.Errorf("/health leaked %q:\n%s", forbidden, health)
		}
	}
}
