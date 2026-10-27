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
	svc := entities.NewService(entities.Deps{HA: client, Permissions: perms, Audit: auditLog, Logger: logger, Now: time.Now})
	bot, err := New(config.Discord{
		Token: "discord-token-for-tests", GuildID: guildID, AdminUserIDs: []string{adminID}, AllowedUserIDs: []string{userID},
	}, commands.Deps{Entities: svc, Audit: auditLog, HA: client, DB: db, Logger: logger, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{bot: bot, api: &fakeAPI{}, ha: ha, perms: perms, audit: auditLog}
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

	req := newRequest(auth.Actor{UserID: userID, Level: auth.LevelUser}, data)

	if req.Command != "permissions" || req.Subcommand != "allow" || req.Args["entity"] != "light.x" || req.Args["page"] != "3" || req.Focused != "page" {
		t.Errorf("request = %+v", req)
	}
}
