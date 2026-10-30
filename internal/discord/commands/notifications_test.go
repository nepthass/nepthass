package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/homeassistant/rest"
	"github.com/nepthass/nepthass/internal/monitor"
	"github.com/nepthass/nepthass/internal/notify"
)

type fakeNotifications struct {
	calls  []string
	err    error
	rules  []RuleItem
	status NotificationStatus
	line   string
}

func (f *fakeNotifications) rec(s string, a ...any) { f.calls = append(f.calls, fmt.Sprintf(s, a...)) }
func (f *fakeNotifications) Config(context.Context, auth.Actor) (notify.Config, error) {
	return notify.Config{}, f.err
}
func (f *fakeNotifications) SetChannel(_ context.Context, _ auth.Actor, id string) (string, error) {
	f.rec("channel %s", id)
	return "alerts", f.err
}
func (f *fakeNotifications) SetPaused(_ context.Context, _ auth.Actor, p bool) error {
	f.rec("paused %v", p)
	return f.err
}
func (f *fakeNotifications) AddRule(_ context.Context, _ auth.Actor, n notify.NewRule) (RuleItem, error) {
	th := "-"
	if n.Threshold != nil {
		th = fmt.Sprint(*n.Threshold)
	}
	cd := "default"
	if n.Cooldown != nil {
		cd = n.Cooldown.String()
	}
	f.rec("add %s %s %s %s", n.Entity, n.Kind, th, cd)
	return RuleItem{RuleView: notify.RuleView{Rule: notify.Rule{ID: 7, Entity: n.Entity, Kind: n.Kind, Threshold: n.Threshold, Cooldown: n.Cooldown}, Category: monitor.CategoryDoor}, Name: "Front door"}, f.err
}
func (f *fakeNotifications) Rules(context.Context, auth.Actor) ([]RuleItem, error) {
	return f.rules, f.err
}
func (f *fakeNotifications) SetRuleEnabled(_ context.Context, _ auth.Actor, id int64, e bool) error {
	f.rec("rule %d enabled=%v", id, e)
	return f.err
}
func (f *fakeNotifications) RemoveRule(_ context.Context, _ auth.Actor, id int64) error {
	f.rec("remove %d", id)
	return f.err
}
func (f *fakeNotifications) SendTest(context.Context, auth.Actor) error {
	f.rec("test")
	return f.err
}
func (f *fakeNotifications) Status(context.Context, auth.Actor) (NotificationStatus, error) {
	return f.status, f.err
}
func (f *fakeNotifications) HealthLine() string { return f.line }

type fakeMon struct {
	synced bool
	views  []monitor.View
}

func (m fakeMon) Status() monitor.Status { return monitor.Status{Synchronized: m.synced} }
func (m fakeMon) Sensors(_ context.Context, _ auth.Actor, page int) (monitor.Page, error) {
	return monitor.Page{Views: m.views, Page: page, Pages: 1, Total: len(m.views)}, nil
}

type fakeHA struct{}

func (fakeHA) Info(context.Context) (rest.Info, error) { return rest.Info{Version: "2026.10"}, nil }

type okDB struct{}

func (okDB) PingContext(context.Context) error { return nil }

var (
	adm = auth.Actor{UserID: "900000000000000001", Level: auth.LevelAdmin}
	usr = auth.Actor{UserID: "900000000000000002", Level: auth.LevelUser}
)

func notifyReg(n Notifications, mon Monitoring) *Registry {
	return New(Deps{Notifications: n, Monitoring: mon, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Entities: nil,
		HA: fakeHA{}, DB: okDB{}, Gateway: func() Gateway { return Gateway{Connected: true} }})
}

func runN(r *Registry, a auth.Actor, sub string, args map[string]string) Reply {
	return r.Run(context.Background(), Request{Actor: a, Command: "notifications", Subcommand: sub, Args: args})
}

func TestNotificationsCommand_isRegisteredOnlyWhenConfigured_withTheRightDefinition(t *testing.T) {
	without := notifyReg(nil, nil)
	for _, d := range without.Definitions() {
		if d.Name == "notifications" {
			t.Fatal("without the dependency the command must not exist (v0.3.0 behaviour)")
		}
	}
	var names []string
	for _, d := range without.Definitions() {
		names = append(names, d.Name)
	}
	if got := strings.Join(names, ","); got != "ping,health,devices,status,control,permissions,audit,setup,sync,sensors" {
		t.Errorf("existing commands changed: %s", got)
	}

	r := notifyReg(&fakeNotifications{}, nil)
	var def *discordgo.ApplicationCommand
	for _, d := range r.Definitions() {
		if d.Name == "notifications" {
			def = d
		}
	}
	if def == nil || def.Contexts == nil || len(*def.Contexts) != 1 || (*def.Contexts)[0] != discordgo.InteractionContextGuild {
		t.Fatalf("a guild-only command: %+v", def)
	}
	var subs []string
	for _, o := range def.Options {
		subs = append(subs, o.Name)
	}
	if got := strings.Join(subs, ","); got != "channel,add,list,pause,resume,remove,test,status" {
		t.Errorf("subcommands = %s", got)
	}
	ch := def.Options[0].Options[0]
	if ch.Type != discordgo.ApplicationCommandOptionChannel || !ch.Required || len(ch.ChannelTypes) != 1 || ch.ChannelTypes[0] != discordgo.ChannelTypeGuildText {
		t.Errorf("the channel option is restricted to text channels: %+v", ch)
	}
	add := def.Options[1].Options
	if add[0].Name != "entity" || !add[0].Autocomplete || add[1].Type != discordgo.ApplicationCommandOptionNumber || add[2].Type != discordgo.ApplicationCommandOptionNumber ||
		add[1].MaxValue != notify.MaxThreshold || add[3].Type != discordgo.ApplicationCommandOptionInteger || add[3].MaxValue != 1440 {
		t.Errorf("add options: %+v", add)
	}
	for _, i := range []int{3, 4, 5} {
		if o := def.Options[i].Options[0]; o.Name != "rule" || !o.Autocomplete {
			t.Errorf("subcommand %s needs an autocompleted rule option: %+v", def.Options[i].Name, o)
		}
	}
}

func TestNotificationsCommand_onlyAdministratorsCanConfigureOrAutocomplete(t *testing.T) {
	f := &fakeNotifications{rules: []RuleItem{{RuleView: notify.RuleView{Rule: notify.Rule{ID: 1}}, Name: "x"}}}
	r := notifyReg(f, fakeMon{synced: true, views: []monitor.View{{Sensor: monitor.Sensor{ID: "sensor.hidden", Name: "Secret"}}}})
	for _, a := range []auth.Actor{usr, {UserID: "3", Level: auth.LevelNone}} {
		for _, s := range []string{"channel", "add", "list", "pause", "resume", "remove", "test", "status"} {
			rep := runN(r, a, s, map[string]string{"channel": "1", "entity": "sensor.x", "rule": "1"})
			if !strings.Contains(strings.ToLower(rep.Content), "administrator") && !strings.Contains(strings.ToLower(rep.Content), "not authorized") {
				t.Errorf("%s as level %d must be refused: %q", s, a.Level, rep.Content)
			}
		}
		if ch := r.Complete(context.Background(), Request{Actor: a, Command: "notifications", Subcommand: "add", Focused: "entity", Args: map[string]string{"entity": ""}}); ch != nil {
			t.Errorf("hidden sensors must not be offered to level %d: %v", a.Level, ch)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("no refused request may reach the service: %v", f.calls)
	}
}

func TestNotificationsCommand_aRequestWithAnUnsupportedOptionIsRefusedNotRunWithoutIt(t *testing.T) {
	f := &fakeNotifications{}
	r := notifyReg(f, nil)
	rep := r.Run(context.Background(), Request{Actor: adm, Command: "notifications", Subcommand: "remove", Args: map[string]string{}, BadOptions: []string{"rule"}})
	if !strings.Contains(rep.Content, "does not understand") || len(f.calls) != 0 {
		t.Errorf("%q %v", rep.Content, f.calls)
	}
	if ch := r.Complete(context.Background(), Request{Actor: adm, Command: "notifications", Focused: "rule", BadOptions: []string{"x"}}); ch != nil {
		t.Error("no autocomplete for a request that cannot be parsed")
	}
	// existing commands are guarded the same way
	if rep := r.Run(context.Background(), Request{Actor: adm, Command: "ping", BadOptions: []string{"x"}}); !strings.Contains(rep.Content, "does not understand") {
		t.Errorf("ping: %q", rep.Content)
	}
}

func TestNotificationsCommand_add(t *testing.T) {
	f := &fakeNotifications{}
	r := notifyReg(f, fakeMon{synced: false})
	if rep := runN(r, adm, "add", map[string]string{"entity": "binary_sensor.door"}); !strings.Contains(rep.Content, "has not synchronized") || len(f.calls) != 0 {
		t.Errorf("unsynchronized monitoring must be explained and nothing attempted: %q %v", rep.Content, f.calls)
	}
	r = notifyReg(f, fakeMon{synced: true})
	if rep := runN(r, adm, "add", map[string]string{"entity": "x", "above": "1", "below": "2"}); !strings.Contains(rep.Content, "not both") || len(f.calls) != 0 {
		t.Errorf("above and below together: %q", rep.Content)
	}
	for _, bad := range []string{"-1", "1441", "abc"} {
		if rep := runN(r, adm, "add", map[string]string{"entity": "x", "cooldown": bad}); !strings.Contains(rep.Content, "between 0 and 1440") || len(f.calls) != 0 {
			t.Errorf("cooldown %q: %q", bad, rep.Content)
		}
	}
	if rep := runN(r, adm, "add", map[string]string{"entity": "sensor.t", "above": "abc"}); !strings.Contains(rep.Content, "not a number") {
		t.Errorf("%q", rep.Content)
	}
	rep := runN(r, adm, "add", map[string]string{"entity": "sensor.t", "above": "28.5", "cooldown": "0"})
	if len(f.calls) != 1 || f.calls[0] != "add sensor.t above 28.5 0s" {
		t.Fatalf("calls = %v", f.calls)
	}
	if !strings.Contains(rep.Content, "Rule #7 added") || !strings.Contains(rep.Content, "Front door") || strings.Contains(rep.Content, "sensor.t") || !strings.Contains(rep.Content, "Cooldown: none") {
		t.Errorf("reply = %q", rep.Content)
	}
	f.calls = nil
	runN(r, adm, "add", map[string]string{"entity": "binary_sensor.door"})
	runN(r, adm, "add", map[string]string{"entity": "sensor.t", "below": "10", "cooldown": "90"})
	if len(f.calls) != 2 || f.calls[0] != "add binary_sensor.door state - default" || f.calls[1] != "add sensor.t below 10 1h30m0s" {
		t.Errorf("kind inference and minutes: %v", f.calls)
	}
}

func TestNotificationsCommand_everyServiceErrorHasAClearMessage_andNeverLeaksInternals(t *testing.T) {
	cases := map[error]string{
		&UserError{Msg: "That channel does not exist."}:             "does not exist",
		notify.ErrNotConfigured:                                     "choose a channel first",
		notify.ErrRuleLimit:                                         "maximum of 200",
		notify.ErrRuleExists:                                        "already has a rule",
		notify.ErrRuleNotFound:                                      "no rule with that number",
		notify.ErrNotShared:                                         "not shared",
		notify.ErrNotMonitored:                                      "not being monitored",
		notify.ErrIncompatible:                                      "does not fit this sensor",
		notify.ErrInvalidThreshold:                                  "finite number",
		notify.ErrInvalidCooldown:                                   "between 0 and 1440",
		errors.New("sql: connection refused to /var/lib/secret.db"): "see the nepthass logs",
	}
	for err, want := range cases {
		f := &fakeNotifications{err: err}
		r := notifyReg(f, fakeMon{synced: true})
		for _, call := range []Reply{runN(r, adm, "add", map[string]string{"entity": "x"}), runN(r, adm, "remove", map[string]string{"rule": "1"}), runN(r, adm, "channel", map[string]string{"channel": "2"})} {
			if got := strings.ToLower(call.Content); !strings.Contains(got, strings.ToLower(want)) || strings.Contains(got, "secret.db") || strings.Contains(got, "sql") {
				t.Errorf("error %q -> %q, want %q", err, call.Content, want)
			}
		}
	}
}

func TestNotificationsCommand_channelWarnsThatEveryoneInTheChannelCanRead_andOtherSubcommands(t *testing.T) {
	f := &fakeNotifications{rules: []RuleItem{
		{RuleView: notify.RuleView{Rule: notify.Rule{ID: 1, Kind: notify.KindState}, Status: notify.StatusActive, Category: monitor.CategoryDoor}, Name: "Front door"},
		{RuleView: notify.RuleView{Rule: notify.Rule{ID: 2, Kind: notify.KindAbove, Threshold: func() *float64 { v := 28.0; return &v }()}, Status: notify.StatusNeedsSharing}, Name: "Living room"},
	}}
	r := notifyReg(f, fakeMon{synced: true})
	rep := runN(r, adm, "channel", map[string]string{"channel": "200000000000000002"})
	if !strings.Contains(rep.Content, "<#200000000000000002>") || !strings.Contains(rep.Content, "Everyone who can read that channel will see these alerts") {
		t.Errorf("channel reply: %q", rep.Content)
	}
	list := runN(r, adm, "list", nil).Embeds[0].Description
	if !strings.Contains(list, "#1") || !strings.Contains(list, "when it opens") || !strings.Contains(list, "active") || !strings.Contains(list, "rises above 28") || !strings.Contains(list, "needs sharing") {
		t.Errorf("list: %q", list)
	}
	runN(r, adm, "pause", map[string]string{"rule": "2"})
	runN(r, adm, "resume", map[string]string{"rule": "2"})
	runN(r, adm, "pause", nil)
	runN(r, adm, "resume", nil)
	runN(r, adm, "remove", map[string]string{"rule": "2"})
	runN(r, adm, "test", nil)
	want := "channel 200000000000000002|rule 2 enabled=false|rule 2 enabled=true|paused true|paused false|remove 2|test"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Errorf("calls = %s", got)
	}
	f.status = NotificationStatus{Config: notify.Config{Configured: true, Settings: notify.Settings{ChannelID: "200000000000000002", Enabled: true}},
		Rules: map[notify.RuleStatus]int{notify.StatusActive: 3, notify.StatusNeedsSharing: 1}, LastDelivery: time.Now(),
		Health: notify.Health{Degraded: true, Reasons: []string{"alerts were dropped recently"}, BreakerOpen: true, DownState: "down_alerted", DroppedOverflow: 4}}
	e := runN(r, adm, "status", nil).Embeds[0]
	var all strings.Builder
	for _, fl := range e.Fields {
		all.WriteString(fl.Name + ": " + fl.Value + "\n")
	}
	for _, w := range []string{"Enabled", "<#200000000000000002>", "3 active", "1 need sharing", "last successful delivery", "overflow 4", "Degraded: alerts were dropped recently", "circuit breaker is open", "an alert was sent"} {
		if !strings.Contains(all.String(), w) {
			t.Errorf("status is missing %q:\n%s", w, all.String())
		}
	}
}

func TestNotificationsCommand_autocompleteOffersOnlyWhatTheReaderReturns_withoutEntityIDsInNames(t *testing.T) {
	views := []monitor.View{
		{Sensor: monitor.Sensor{ID: "binary_sensor.front", Category: monitor.CategoryDoor, Name: "Front door"}},
		{Sensor: monitor.Sensor{ID: "sensor.temp", Category: monitor.CategoryTemperature}, Alias: "Living room"},
	}
	f := &fakeNotifications{rules: []RuleItem{{RuleView: notify.RuleView{Rule: notify.Rule{ID: 5, Kind: notify.KindState}, Status: notify.StatusPaused, Category: monitor.CategoryDoor}, Name: "Front door"}}}
	r := notifyReg(f, fakeMon{synced: true, views: views})
	all := r.Complete(context.Background(), Request{Actor: adm, Command: "notifications", Subcommand: "add", Focused: "entity", Args: map[string]string{"entity": ""}})
	if len(all) != 2 || all[0].Value != "binary_sensor.front" || !strings.Contains(all[0].Name, "Front door") || !strings.Contains(all[1].Name, "Living room") {
		t.Fatalf("choices = %+v", all)
	}
	for _, c := range all {
		if strings.Contains(c.Name, "binary_sensor") || strings.Contains(c.Name, "sensor.temp") {
			t.Errorf("a choice name must not show the entity ID: %q", c.Name)
		}
	}
	if q := r.Complete(context.Background(), Request{Actor: adm, Command: "notifications", Subcommand: "add", Focused: "entity", Args: map[string]string{"entity": "living"}}); len(q) != 1 {
		t.Errorf("the typed text filters: %+v", q)
	}
	rules := r.Complete(context.Background(), Request{Actor: adm, Command: "notifications", Subcommand: "remove", Focused: "rule", Args: map[string]string{"rule": ""}})
	if len(rules) != 1 || rules[0].Value != int64(5) || !strings.Contains(rules[0].Name, "#5") {
		t.Errorf("rule choices: %+v", rules)
	}
}

func TestHealth_showsTheNotificationLineOnlyWhenNotificationsAreConfigured(t *testing.T) {
	fields := func(r *Registry) string {
		rep := r.Run(context.Background(), Request{Actor: usr, Command: "health"})
		var s strings.Builder
		for _, f := range rep.Embeds[0].Fields {
			s.WriteString(f.Name + "=" + f.Value + "|")
		}
		return s.String()
	}
	if got := fields(notifyReg(nil, nil)); strings.Contains(got, "Notifications") {
		t.Errorf("v0.3.0 /health is unchanged without the dependency: %s", got)
	}
	got := fields(notifyReg(&fakeNotifications{line: "Degraded: delivery backlog"}, nil))
	if !strings.Contains(got, "Notifications=Degraded: delivery backlog") || strings.Contains(got, "<#") {
		t.Errorf("any user may see a non-sensitive line: %s", got)
	}
}

func TestNotificationsCommand_listingsAndStatusNeverShowEntityIDs(t *testing.T) {
	f := &fakeNotifications{rules: []RuleItem{{RuleView: notify.RuleView{Rule: notify.Rule{ID: 3, Entity: "binary_sensor.secret_vault", Kind: notify.KindState}, Status: notify.StatusNeedsSharing, Category: monitor.CategoryDoor}, Name: "Vault door"}}}
	r := notifyReg(f, fakeMon{synced: true})
	list := runN(r, adm, "list", nil).Embeds[0].Description
	choices := r.Complete(context.Background(), Request{Actor: adm, Command: "notifications", Subcommand: "pause", Focused: "rule", Args: map[string]string{"rule": ""}})
	var seen strings.Builder
	seen.WriteString(list)
	for _, c := range choices {
		seen.WriteString(c.Name)
	}
	for _, e := range runN(r, adm, "status", nil).Embeds[0].Fields {
		seen.WriteString(e.Name + e.Value)
	}
	if got := seen.String(); strings.Contains(got, "secret_vault") || strings.Contains(got, "binary_sensor") || !strings.Contains(got, "Vault door") {
		t.Errorf("rules are shown by name only: %q", got)
	}
}
