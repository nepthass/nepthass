package discord

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/config"
	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/monitor"
	"github.com/nepthass/nepthass/internal/notify"
)

const strangerID = "500000000000000009"

// authFake counts every call that reaches the notification service.
type authFake struct {
	mu    sync.Mutex
	calls int
}

func (f *authFake) hit() { f.mu.Lock(); f.calls++; f.mu.Unlock() }
func (f *authFake) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}
func (f *authFake) Config(context.Context, auth.Actor) (notify.Config, error) {
	f.hit()
	return notify.Config{}, nil
}
func (f *authFake) SetChannel(context.Context, auth.Actor, string) (string, error) {
	f.hit()
	return "alerts", nil
}
func (f *authFake) SetPaused(context.Context, auth.Actor, bool) error { f.hit(); return nil }
func (f *authFake) AddRule(context.Context, auth.Actor, notify.NewRule) (commands.RuleItem, error) {
	f.hit()
	return commands.RuleItem{}, nil
}
func (f *authFake) Rules(context.Context, auth.Actor) ([]commands.RuleItem, error) {
	f.hit()
	return []commands.RuleItem{{RuleView: notify.RuleView{Rule: notify.Rule{ID: 1, Kind: notify.KindState}, Status: notify.StatusActive, Category: monitor.CategoryDoor}, Name: "Front door"}}, nil
}
func (f *authFake) SetRuleEnabled(context.Context, auth.Actor, int64, bool) error {
	f.hit()
	return nil
}
func (f *authFake) RemoveRule(context.Context, auth.Actor, int64) error { f.hit(); return nil }
func (f *authFake) SendTest(context.Context, auth.Actor) error          { f.hit(); return nil }
func (f *authFake) Status(context.Context, auth.Actor) (commands.NotificationStatus, error) {
	f.hit()
	return commands.NotificationStatus{}, nil
}
func (f *authFake) HealthLine() string { return "Enabled" }

func newAuthBot(t *testing.T, fake *authFake) *Bot {
	t.Helper()
	bot, err := New(config.Discord{Token: "discord-token-for-tests", GuildID: guildID, AdminUserIDs: []string{adminID}, AllowedUserIDs: []string{userID}},
		commands.Deps{Notifications: fake, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return bot
}

func complete(guild, user, command, subcmd, focused string) *discordgo.Interaction {
	opt := &discordgo.ApplicationCommandInteractionDataOption{Name: focused, Type: discordgo.ApplicationCommandOptionInteger, Value: "", Focused: true}
	return &discordgo.Interaction{Type: discordgo.InteractionApplicationCommandAutocomplete, GuildID: guild, Member: &discordgo.Member{User: &discordgo.User{ID: user}},
		Data: discordgo.ApplicationCommandInteractionData{Name: command, Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: subcmd, Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandInteractionDataOption{opt}}}}}
}

func TestInteraction_notificationsCommandsAreAdministratorOnly_forEveryIdentity(t *testing.T) {
	fake, api := &authFake{}, &fakeAPI{}
	ctx := context.Background()
	// A fresh bot per request, because the bot rate-limits bursts per user.
	serve := func(i *discordgo.Interaction) {
		api.reset()
		newAuthBot(t, fake).serve(ctx, api, i)
	}
	subs := []string{"channel", "add", "list", "pause", "resume", "remove", "test", "status"}
	args := map[string][]*discordgo.ApplicationCommandInteractionDataOption{"channel": {{Name: "channel", Type: discordgo.ApplicationCommandOptionChannel, Value: "200000000000000002"}},
		"add": {str("entity", "binary_sensor.door")}, "remove": {{Name: "rule", Type: discordgo.ApplicationCommandOptionInteger, Value: float64(1)}}}
	for _, who := range []struct {
		name, id, want string
	}{{"allowed normal user", userID, "administrators"}, {"stranger", strangerID, "not authorized"}} {
		for _, s := range subs {
			serve(slash(guildID, who.id, "notifications", sub(s, args[s]...)))
			if got := strings.ToLower(api.finalText()); !strings.Contains(got, who.want) {
				t.Errorf("%s running %s: reply %q, want it to mention %q", who.name, s, got, who.want)
			}
		}
		serve(complete(guildID, who.id, "notifications", "remove", "rule"))
		if len(api.responses) != 1 || api.responses[0].Data == nil || len(api.responses[0].Data.Choices) != 0 {
			t.Errorf("%s must get no autocomplete suggestions: %+v", who.name, api.responses)
		}
	}
	if fake.n() != 0 {
		t.Fatalf("nothing from a non-administrator may reach the notification service: %d calls", fake.n())
	}

	serve(slash(otherGuild, adminID, "notifications", sub("status"))) // the right person in the wrong server
	if fake.n() != 0 {
		t.Error("an administrator ID used in another guild must be refused")
	}
	serve(slash(guildID, adminID, "notifications", sub("status")))
	serve(complete(guildID, adminID, "notifications", "remove", "rule"))
	if fake.n() < 2 || len(api.responses) != 1 || api.responses[0].Data == nil || len(api.responses[0].Data.Choices) != 1 {
		t.Errorf("the administrator reaches the service and gets suggestions: calls=%d responses=%+v", fake.n(), api.responses)
	}

	// An option type NeptHass cannot parse is refused, never run without the option.
	before := fake.n()
	serve(slash(guildID, adminID, "notifications", sub("test", &discordgo.ApplicationCommandInteractionDataOption{Name: "flag", Type: discordgo.ApplicationCommandOptionBoolean, Value: true})))
	if got := api.finalText(); !strings.Contains(got, "does not understand") || fake.n() != before {
		t.Errorf("unsupported option types are refused: %q calls %d->%d", got, before, fake.n())
	}
}
