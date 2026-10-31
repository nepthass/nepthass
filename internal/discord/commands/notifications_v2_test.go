package commands

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
	"github.com/nepthass/nepthass/internal/notify"
)

func text(r Reply) string {
	var b strings.Builder
	b.WriteString(r.Content)
	for _, e := range r.Embeds {
		b.WriteString(e.Description)
		for _, f := range e.Fields {
			b.WriteString("\n" + f.Name + ": " + f.Value)
		}
	}
	return b.String()
}

func TestNotificationsAdd_passesDurationAndClearThrough_andSaysWhatItDoes(t *testing.T) {
	f := &fakeNotifications{item: &RuleItem{RuleView: notify.RuleView{Rule: notify.Rule{ID: 12, Unit: "°C"}, Category: monitor.CategoryTemperature}, Name: "Attic"}}
	r := notifyReg(f, fakeMon{synced: true})
	got := text(runN(r, adm, "add", map[string]string{"entity": "sensor.attic", "above": "30", "clear": "28", "for": "5"}))
	if len(f.added) != 1 || f.added[0].Kind != notify.KindAbove || *f.added[0].Threshold != 30 || *f.added[0].Clear != 28 || f.added[0].Duration != 5*time.Minute {
		t.Fatalf("the command must hand the service exactly what was typed: %+v", f.added)
	}
	for _, want := range []string{"Rule #12 added", "rises above 30 °C", "can alert again only after it is below 28 °C", "stays that way for 5 min"} {
		if !strings.Contains(got, want) {
			t.Errorf("reply %q must contain %q", got, want)
		}
	}
	d := &fakeNotifications{item: &RuleItem{RuleView: notify.RuleView{Category: monitor.CategoryGarageDoor}, Name: "Garage"}}
	got = text(runN(notifyReg(d, fakeMon{synced: true}), adm, "add", map[string]string{"entity": "binary_sensor.garage", "for": "90"}))
	if d.added[0].Duration != 90*time.Minute || !strings.Contains(got, "when it opens, and stays that way for 1 h 30 min") {
		t.Errorf("a state rule with a duration: %+v %q", d.added, got)
	}
	b := &fakeNotifications{item: &RuleItem{RuleView: notify.RuleView{Category: monitor.CategoryTemperature, Rule: notify.Rule{Unit: "°F"}}, Name: "Cellar"}}
	got = text(runN(notifyReg(b, fakeMon{synced: true}), adm, "add", map[string]string{"entity": "sensor.cellar", "below": "40", "clear": "45"}))
	if !strings.Contains(got, "falls below 40 °F (can alert again only after it is above 45 °F)") {
		t.Errorf("a below rule is mirrored: %q", got)
	}
}

func TestNotificationsAdd_validatesDurationAndClearOnTheServer(t *testing.T) {
	for _, in := range []map[string]string{{"for": "0"}, {"for": "1441"}, {"for": "-5"}, {"for": "abc"}, {"for": "2.5"}, {"for": ""}, {"above": "1", "clear": "x"}, {"above": "1", "clear": ""}} {
		f := &fakeNotifications{}
		in["entity"] = "binary_sensor.door"
		if _, set := in["above"]; !set {
			in["entity"] = "binary_sensor.door"
		}
		got := text(runN(notifyReg(f, fakeMon{synced: true}), adm, "add", in))
		if len(f.added) != 0 || !strings.Contains(got, "between 1 and 1440 minutes") && !strings.Contains(got, "is not a number") {
			t.Errorf("%v: the client's limits are not trusted: added=%d reply=%q", in, len(f.added), got)
		}
	}
}

func TestNotificationsAdd_everyServiceRefusalHasAClearMessage_andNeverEchoesTheEntity(t *testing.T) {
	cases := map[error]string{
		notify.ErrHazardImmediate:    "always immediate",
		notify.ErrInvalidDuration:    "between 1 and 1440 minutes",
		notify.ErrDurationNotAllowed: "does not support waiting",
		notify.ErrInvalidClear:       "recovery side of the threshold",
		notify.ErrClearNotAllowed:    "only applies to a numeric rule",
		notify.ErrUnitUnsupported:    "reports no unit, or a unit NeptHass does not support",
		notify.ErrUnitBound:          "never rebound",
		notify.ErrRuleExists:         "kind and duration",
	}
	for cause, want := range cases {
		f := &fakeNotifications{err: fmt.Errorf("wrapped: %w", cause)}
		got := text(runN(notifyReg(f, fakeMon{synced: true}), adm, "add", map[string]string{"entity": "binary_sensor.secret_leak"}))
		if !strings.Contains(got, want) || strings.Contains(got, "secret_leak") || strings.Contains(got, "wrapped") {
			t.Errorf("%v -> %q, want a message containing %q and no entity or internal text", cause, got, want)
		}
	}
}

func TestNotificationsAdd_tellsTheAdministratorWhenTheSensorIsAlreadyActive(t *testing.T) {
	mk := func(cat monitor.Category, d time.Duration) string {
		f := &fakeNotifications{item: &RuleItem{RuleView: notify.RuleView{Category: cat}, Name: "S", AlreadyActive: true}}
		args := map[string]string{"entity": "binary_sensor.s"}
		if d > 0 {
			args["for"] = fmt.Sprint(int(d / time.Minute))
		}
		return text(runN(notifyReg(f, fakeMon{synced: true}), adm, "add", args))
	}
	if got := mk(monitor.CategoryLeak, 0); !strings.Contains(got, "critical alert") || !strings.Contains(got, "on its way") {
		t.Errorf("a leak that is already wet: %q", got)
	}
	if got := mk(monitor.CategoryDoor, 10*time.Minute); !strings.Contains(got, "counts the duration from now") {
		t.Errorf("a duration rule on an open door: %q", got)
	}
	if got := mk(monitor.CategoryDoor, 0); !strings.Contains(got, "nothing is sent for it") || !strings.Contains(got, "next time") {
		t.Errorf("a plain rule on an open door: %q", got)
	}
}

func TestNotificationsList_showsDurationsUnitsAndExplainsDormantRules(t *testing.T) {
	th, clr := 30.0, 28.0
	f := &fakeNotifications{rules: []RuleItem{
		{RuleView: notify.RuleView{Rule: notify.Rule{ID: 1, Entity: "sensor.a", Kind: notify.KindAbove, Threshold: &th, Clear: &clr, Duration: 5 * time.Minute, Unit: "°C"}, Status: notify.StatusActive, Category: monitor.CategoryTemperature}, Name: "Attic"},
		{RuleView: notify.RuleView{Rule: notify.Rule{ID: 2, Entity: "sensor.b", Kind: notify.KindBelow, Threshold: &th}, Status: notify.StatusUnitUnresolved, Category: monitor.CategoryTemperature}, Name: "Cellar"},
		{RuleView: notify.RuleView{Rule: notify.Rule{ID: 3, Entity: "sensor.c", Kind: notify.KindAbove, Threshold: &th, Unit: "°C"}, Status: notify.StatusUnitChanged, Category: monitor.CategoryTemperature}, Name: "Shed"},
		{RuleView: notify.RuleView{Rule: notify.Rule{ID: 4, Entity: "binary_sensor.leak", Kind: notify.KindState}, Status: notify.StatusActive, Category: monitor.CategoryLeak}, Name: "Basement"},
	}}
	got := text(runN(notifyReg(f, nil), adm, "list", nil))
	for _, want := range []string{"rises above 30 °C (can alert again only after it is below 28 °C), and stays that way for 5 min - active",
		"**#2** Cellar", "NOT EVALUATING: created before units were tracked; confirm its unit with /notifications unit",
		"NOT EVALUATING: the sensor now reports a different unit", "when it gets wet - active"} {
		if !strings.Contains(got, want) {
			t.Errorf("list %q must contain %q", got, want)
		}
	}
	if strings.Contains(got, "sensor.a") || strings.Contains(got, "binary_sensor.leak") {
		t.Error("the list names sensors by display name, never by entity ID")
	}
}

func TestNotificationsList_staysInsideTheEmbedLimitWithManyLongRules(t *testing.T) {
	th := 30.5
	var rules []RuleItem
	for i := 1; i <= notify.MaxRules; i++ {
		rules = append(rules, RuleItem{RuleView: notify.RuleView{Rule: notify.Rule{ID: int64(i), Entity: entities.ID(fmt.Sprintf("sensor.s%d", i)), Kind: notify.KindAbove, Threshold: &th, Clear: &th, Duration: 90 * time.Minute, Unit: "°C"},
			Status: notify.StatusUnitUnresolved, Category: monitor.CategoryTemperature}, Name: strings.Repeat("Name ", 20)})
	}
	got := runN(notifyReg(&fakeNotifications{rules: rules}, nil), adm, "list", nil)
	d := got.Embeds[0].Description
	if utf8.RuneCountInString(d) > 4000 || !strings.Contains(d, "...and ") || strings.HasSuffix(d, "…") {
		t.Fatalf("a long list stops on a whole line and says how many more there are (%d chars): ...%q", utf8.RuneCountInString(d), d[max(0, len(d)-120):])
	}
}

func TestNotificationsUnit_bindsARuleAndExplainsWhatItMeans(t *testing.T) {
	f := &fakeNotifications{unit: "°F"}
	r := notifyReg(f, nil)
	got := text(runN(r, adm, "unit", map[string]string{"rule": "4"}))
	if len(f.calls) != 1 || f.calls[0] != "unit 4" || !strings.Contains(got, "Rule #4 is now bound to **°F**") || !strings.Contains(got, "delete the rule and add it again") {
		t.Errorf("calls=%v reply=%q", f.calls, got)
	}
	if got := text(runN(r, adm, "unit", map[string]string{"rule": "x"})); !strings.Contains(got, "not a rule number") || len(f.calls) != 1 {
		t.Errorf("a bad rule number never reaches the service: %q", got)
	}
	for cause, want := range map[error]string{notify.ErrUnitBound: "never rebound", notify.ErrUnitUnsupported: "reports no unit", notify.ErrRuleNotFound: "no rule with that number", notify.ErrNotMonitored: "not being monitored", notify.ErrIncompatible: "does not fit"} {
		g := &fakeNotifications{err: cause}
		if got := text(runN(notifyReg(g, nil), adm, "unit", map[string]string{"rule": "4"})); !strings.Contains(got, want) {
			t.Errorf("%v: %q must contain %q", cause, got, want)
		}
	}
	u := &fakeNotifications{}
	if got := text(runN(notifyReg(u, nil), usr, "unit", map[string]string{"rule": "4"})); len(u.calls) != 0 || !strings.Contains(got, "administrators") {
		t.Errorf("only an administrator can confirm a unit: calls=%v reply=%q", u.calls, got)
	}
}

func TestNotificationsAutocomplete_offersOnlyWhatAppliesToTheChoice(t *testing.T) {
	th := 30.0
	f := &fakeNotifications{rules: []RuleItem{
		{RuleView: notify.RuleView{Rule: notify.Rule{ID: 1, Kind: notify.KindAbove, Threshold: &th, Unit: "°C"}, Status: notify.StatusActive, Category: monitor.CategoryTemperature}, Name: "Attic"},
		{RuleView: notify.RuleView{Rule: notify.Rule{ID: 2, Kind: notify.KindAbove, Threshold: &th}, Status: notify.StatusUnitUnresolved, Category: monitor.CategoryTemperature}, Name: "Cellar"},
	}}
	mon := fakeMon{synced: true, views: []monitor.View{
		{Sensor: monitor.Sensor{ID: "binary_sensor.leak", Category: monitor.CategoryLeak, Name: "Basement"}},
		{Sensor: monitor.Sensor{ID: "sensor.attic", Category: monitor.CategoryTemperature, Name: "Attic", Unit: "°C"}},
		{Sensor: monitor.Sensor{ID: "binary_sensor.door", Category: monitor.CategoryDoor, Name: "Door"}},
	}}
	r := notifyReg(f, mon)
	ch := r.Complete(context.Background(), Request{Actor: adm, Command: "notifications", Subcommand: "add", Focused: "entity", Args: map[string]string{"entity": ""}})
	labels := ""
	for _, c := range ch {
		labels += c.Name + "|"
	}
	for _, want := range []string{"Basement (Water leak, immediate alerts only)|", "Attic (Temperature, °C)|", "Door (Door)|"} {
		if !strings.Contains(labels, want) {
			t.Errorf("the entity choices say what each sensor supports: %q must contain %q", labels, want)
		}
	}
	unit := r.Complete(context.Background(), Request{Actor: adm, Command: "notifications", Subcommand: "unit", Focused: "rule", Args: map[string]string{"rule": ""}})
	if len(unit) != 1 || unit[0].Value != int64(2) {
		t.Errorf("/notifications unit offers only the rule that needs its unit confirmed: %+v", unit)
	}
	rm := r.Complete(context.Background(), Request{Actor: adm, Command: "notifications", Subcommand: "remove", Focused: "rule", Args: map[string]string{"rule": ""}})
	if len(rm) != 2 {
		t.Errorf("remove still offers every rule: %d", len(rm))
	}
}

func TestNotificationsStatus_showsDurationStartupAndUnitInformation(t *testing.T) {
	f := &fakeNotifications{status: NotificationStatus{
		Config: notify.Config{Configured: true, Settings: notify.Settings{ChannelID: "200000000000000002", Enabled: true}},
		Rules:  map[notify.RuleStatus]int{notify.StatusActive: 3, notify.StatusUnitUnresolved: 2, notify.StatusUnitChanged: 1},
		Health: notify.Health{Active: true, PendingEpisodes: 4, EpisodesFired: 7, EpisodesCancelled: 9, InitialAlerts: 1, StartupListed: 5, StartupSummaries: 1, SummarySuppressed: 2, MarkSuppressed: 3},
	}}
	got := text(runN(notifyReg(f, nil), adm, "status", nil))
	for _, want := range []string{"4 counting now; 7 alerted, 9 ended without alerting", "1 hazard alert(s) for conditions already present", "5 sensor(s) listed in the start-up summary (1 sent, 2 held back", "3 hazard alert(s) skipped because the same one was already delivered",
		"2 rule(s) need their unit confirmed", "/notifications unit", "1 rule(s) are paused because their sensor changed unit"} {
		if !strings.Contains(got, want) {
			t.Errorf("status %q must contain %q", got, want)
		}
	}
}

var discordName = regexp.MustCompile(`^[-_\p{Ll}\p{N}]{1,32}$`)

// Discord rejects a command that breaks these limits; the whole definition must stay inside them.
func TestEveryCommandDefinitionRespectsDiscordsLimits(t *testing.T) {
	r := notifyReg(&fakeNotifications{}, fakeMon{synced: true})
	var total func(o []*discordgo.ApplicationCommandOption) int
	total = func(opts []*discordgo.ApplicationCommandOption) int {
		n := 0
		for _, o := range opts {
			n += len(o.Name) + len(o.Description) + total(o.Options)
			for _, c := range o.Choices {
				n += len(c.Name)
			}
		}
		return n
	}
	var check func(path string, opts []*discordgo.ApplicationCommandOption)
	check = func(path string, opts []*discordgo.ApplicationCommandOption) {
		if len(opts) > 25 {
			t.Errorf("%s has %d options (max 25)", path, len(opts))
		}
		optional := false
		for _, o := range opts {
			if !discordName.MatchString(o.Name) {
				t.Errorf("%s/%s: not a valid Discord name", path, o.Name)
			}
			if d := utf8.RuneCountInString(o.Description); d < 1 || d > 100 {
				t.Errorf("%s/%s: description is %d characters (1 to 100)", path, o.Name, d)
			}
			if o.Type != discordgo.ApplicationCommandOptionSubCommand && o.Type != discordgo.ApplicationCommandOptionSubCommandGroup {
				if o.Required && optional {
					t.Errorf("%s/%s: a required option may not follow an optional one", path, o.Name)
				}
				optional = optional || !o.Required
			}
			if len(o.Choices) > 25 {
				t.Errorf("%s/%s: %d choices", path, o.Name, len(o.Choices))
			}
			if o.MinValue != nil && o.MaxValue != 0 && *o.MinValue > o.MaxValue {
				t.Errorf("%s/%s: min above max", path, o.Name)
			}
			check(path+"/"+o.Name, o.Options)
		}
	}
	for _, d := range r.Definitions() {
		if !discordName.MatchString(d.Name) || utf8.RuneCountInString(d.Description) < 1 || utf8.RuneCountInString(d.Description) > 100 {
			t.Errorf("command %s: name or description out of limits", d.Name)
		}
		check(d.Name, d.Options)
		if n := total(d.Options) + len(d.Name) + len(d.Description); n > 8000 {
			t.Errorf("command %s: %d characters of names, descriptions and choices (max 8000)", d.Name, n)
		}
	}
}

func TestNotificationsUnit_isAnAdministratorOnlyCommandLikeTheRest(t *testing.T) {
	r := notifyReg(&fakeNotifications{}, nil)
	if lvl, ok := r.Level("notifications"); !ok || lvl != auth.LevelAdmin {
		t.Fatalf("notifications needs an administrator: %v %v", lvl, ok)
	}
	_ = errors.Is
}
