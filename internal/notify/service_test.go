package notify

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
)

var ctx = context.Background()

func TestService_notificationsAreDisabledByDefault(t *testing.T) {
	f := newFixture(t)
	cfg, err := f.svc.Config(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Configured || cfg.Active() || cfg.Rules != 0 || cfg.Settings.Enabled {
		t.Errorf("a fresh install has no settings, so notifications are disabled: %+v", cfg)
	}
	if rules, err := f.svc.Rules(ctx, admin); err != nil || len(rules) != 0 {
		t.Errorf("rules = %v, %v", rules, err)
	}
	for _, a := range []auth.Actor{user, nobody} {
		if _, err := f.svc.Config(ctx, a); !errors.Is(err, auth.ErrAdminRequired) {
			t.Errorf("reading the configuration needs an administrator: %v", err)
		}
		if _, err := f.svc.Rules(ctx, a); !errors.Is(err, auth.ErrAdminRequired) {
			t.Errorf("listing rules needs an administrator: %v", err)
		}
	}
}

func TestService_everyMutationRequiresAnAdministrator_andRefusalsAreAudited(t *testing.T) {
	f := newFixture(t)
	f.ready("binary_sensor.door", monitor.CategoryDoor)
	if err := f.svc.SetChannel(ctx, admin, testChannel); err != nil {
		t.Fatal(err)
	}
	rule, err := f.svc.AddRule(ctx, admin, NewRule{Entity: "binary_sensor.door", Kind: KindState})
	if err != nil {
		t.Fatal(err)
	}
	baseline := len(f.auditRows())

	for _, actor := range []auth.Actor{user, nobody} {
		for name, call := range map[string]func() error{
			"SetChannel": func() error { return f.svc.SetChannel(ctx, actor, "300000000000000003") },
			"SetEnabled": func() error { return f.svc.SetEnabled(ctx, actor, false) },
			"AddRule": func() error {
				_, e := f.svc.AddRule(ctx, actor, NewRule{Entity: "binary_sensor.door", Kind: KindState})
				return e
			},
			"SetRuleEnabled": func() error { return f.svc.SetRuleEnabled(ctx, actor, rule.ID, false) },
			"RemoveRule":     func() error { return f.svc.RemoveRule(ctx, actor, rule.ID) },
		} {
			if err := call(); !errors.Is(err, auth.ErrAdminRequired) {
				t.Errorf("%s as level %d: %v", name, actor.Level, err)
			}
		}
	}
	cfg, _ := f.svc.Config(ctx, admin)
	views, _ := f.svc.Rules(ctx, admin)
	if cfg.Settings.ChannelID != testChannel || !cfg.Settings.Enabled || len(views) != 1 || !views[0].Enabled {
		t.Fatalf("a refused mutation changed the configuration: %+v %+v", cfg, views)
	}
	denied := f.auditRows()[:len(f.auditRows())-baseline]
	if len(denied) != 10 {
		t.Fatalf("every refused attempt must be audited: %d entries", len(denied))
	}
	for _, e := range denied {
		if e.Outcome != audit.OutcomeDenied || e.Detail != auth.ErrAdminRequired.Error() || (e.ActorID != user.UserID && e.ActorID != nobody.UserID) {
			t.Errorf("denied entry = %+v", e)
		}
	}
}

func TestService_setChannel_createsEnabledSettings_keepsTheFlagOnChange_andRejectsMalformedIDs(t *testing.T) {
	f := newFixture(t)
	for _, bad := range []string{"", "123", "not-a-number-SECRET", "12345x6789"} {
		if err := f.svc.SetChannel(ctx, admin, bad); !errors.Is(err, ErrInvalidChannel) {
			t.Errorf("channel %q: %v", bad, err)
		}
	}
	if f.count("notification_settings") != 0 {
		t.Fatal("a malformed channel must not create settings")
	}
	if e := f.lastAudit(); e.Outcome != audit.OutcomeError || e.Detail != ErrInvalidChannel.Error() {
		t.Errorf("audit of a malformed channel = %+v (the input must not be stored)", e)
	}

	if err := f.svc.SetChannel(ctx, admin, testChannel); err != nil {
		t.Fatal(err)
	}
	cfg, _ := f.svc.Config(ctx, admin)
	if !cfg.Configured || !cfg.Active() || cfg.Settings.ChannelID != testChannel || cfg.Settings.GuildID != testGuild || cfg.Settings.UpdatedBy != admin.UserID {
		t.Errorf("config = %+v", cfg)
	}
	if e := f.lastAudit(); e.Outcome != audit.OutcomeSuccess || e.Command != "notifications.channel" || e.Detail != "channel="+testChannel || e.ActorID != admin.UserID {
		t.Errorf("audit = %+v", e)
	}

	if err := f.svc.SetEnabled(ctx, admin, false); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetChannel(ctx, admin, "300000000000000003"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = f.svc.Config(ctx, admin)
	if cfg.Settings.ChannelID != "300000000000000003" || cfg.Settings.Enabled || cfg.Active() {
		t.Errorf("changing the channel must keep notifications disabled: %+v", cfg)
	}
}

func TestService_setEnabled_needsAChannel_andTogglesWithoutLosingConfiguration(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.SetEnabled(ctx, admin, true); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("enabling before a channel is set: %v", err)
	}
	if e := f.lastAudit(); e.Outcome != audit.OutcomeError || e.Detail != ErrNotConfigured.Error() {
		t.Errorf("audit = %+v", e)
	}
	_ = f.svc.SetChannel(ctx, admin, testChannel)
	if err := f.svc.SetEnabled(ctx, admin, false); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := f.svc.Config(ctx, admin); !cfg.Configured || cfg.Active() {
		t.Errorf("disabled but still configured: %+v", cfg)
	}
	if err := f.svc.SetEnabled(ctx, admin, true); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := f.svc.Config(ctx, admin); !cfg.Active() {
		t.Errorf("enabled again: %+v", cfg)
	}
}

func TestService_addRule_validation(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(f *fixture)
		rule    NewRule
		want    error
		outcome audit.Outcome
	}{
		{name: "door state", setup: ready("binary_sensor.d", monitor.CategoryDoor), rule: NewRule{Entity: "binary_sensor.d", Kind: KindState}},
		{name: "window state", setup: ready("binary_sensor.d", monitor.CategoryWindow), rule: NewRule{Entity: "binary_sensor.d", Kind: KindState}},
		{name: "motion state", setup: ready("binary_sensor.d", monitor.CategoryMotion), rule: NewRule{Entity: "binary_sensor.d", Kind: KindState}},
		{name: "leak state", setup: ready("binary_sensor.d", monitor.CategoryLeak), rule: NewRule{Entity: "binary_sensor.d", Kind: KindState}},
		{name: "battery low state", setup: ready("binary_sensor.d", monitor.CategoryBatteryLow), rule: NewRule{Entity: "binary_sensor.d", Kind: KindState}},
		{name: "temperature above", setup: ready("sensor.t", monitor.CategoryTemperature), rule: NewRule{Entity: "sensor.t", Kind: KindAbove, Threshold: fl(28)}},
		{name: "humidity below", setup: ready("sensor.t", monitor.CategoryHumidity), rule: NewRule{Entity: "sensor.t", Kind: KindBelow, Threshold: fl(30)}},
		{name: "battery below", setup: ready("sensor.t", monitor.CategoryBattery), rule: NewRule{Entity: "sensor.t", Kind: KindBelow, Threshold: fl(15)}},
		{name: "explicit zero cooldown", setup: ready("binary_sensor.d", monitor.CategoryDoor), rule: NewRule{Entity: "binary_sensor.d", Kind: KindState, Cooldown: dur(0)}},
		{name: "ninety second cooldown", setup: ready("binary_sensor.d", monitor.CategoryDoor), rule: NewRule{Entity: "binary_sensor.d", Kind: KindState, Cooldown: dur(90 * time.Second)}},
		{name: "state on a numeric sensor", setup: ready("sensor.t", monitor.CategoryTemperature), rule: NewRule{Entity: "sensor.t", Kind: KindState}, want: ErrIncompatible, outcome: audit.OutcomeError},
		{name: "above on a binary sensor", setup: ready("binary_sensor.d", monitor.CategoryDoor), rule: NewRule{Entity: "binary_sensor.d", Kind: KindAbove, Threshold: fl(1)}, want: ErrIncompatible, outcome: audit.OutcomeError},
		{name: "state with a threshold", setup: ready("binary_sensor.d", monitor.CategoryDoor), rule: NewRule{Entity: "binary_sensor.d", Kind: KindState, Threshold: fl(1)}, want: ErrInvalidThreshold, outcome: audit.OutcomeError},
		{name: "above without a threshold", setup: ready("sensor.t", monitor.CategoryTemperature), rule: NewRule{Entity: "sensor.t", Kind: KindAbove}, want: ErrInvalidThreshold, outcome: audit.OutcomeError},
		{name: "NaN threshold", setup: ready("sensor.t", monitor.CategoryTemperature), rule: NewRule{Entity: "sensor.t", Kind: KindAbove, Threshold: fl(math.NaN())}, want: ErrInvalidThreshold, outcome: audit.OutcomeError},
		{name: "positive infinity", setup: ready("sensor.t", monitor.CategoryTemperature), rule: NewRule{Entity: "sensor.t", Kind: KindAbove, Threshold: fl(math.Inf(1))}, want: ErrInvalidThreshold, outcome: audit.OutcomeError},
		{name: "negative infinity", setup: ready("sensor.t", monitor.CategoryTemperature), rule: NewRule{Entity: "sensor.t", Kind: KindBelow, Threshold: fl(math.Inf(-1))}, want: ErrInvalidThreshold, outcome: audit.OutcomeError},
		{name: "threshold out of range", setup: ready("sensor.t", monitor.CategoryTemperature), rule: NewRule{Entity: "sensor.t", Kind: KindAbove, Threshold: fl(MaxThreshold + 1)}, want: ErrInvalidThreshold, outcome: audit.OutcomeError},
		{name: "negative cooldown", setup: ready("binary_sensor.d", monitor.CategoryDoor), rule: NewRule{Entity: "binary_sensor.d", Kind: KindState, Cooldown: dur(-time.Second)}, want: ErrInvalidCooldown, outcome: audit.OutcomeError},
		{name: "fractional cooldown", setup: ready("binary_sensor.d", monitor.CategoryDoor), rule: NewRule{Entity: "binary_sensor.d", Kind: KindState, Cooldown: dur(1500 * time.Millisecond)}, want: ErrInvalidCooldown, outcome: audit.OutcomeError},
		{name: "cooldown over a day", setup: ready("binary_sensor.d", monitor.CategoryDoor), rule: NewRule{Entity: "binary_sensor.d", Kind: KindState, Cooldown: dur(MaxCooldown + time.Second)}, want: ErrInvalidCooldown, outcome: audit.OutcomeError},
		{name: "unknown kind", setup: ready("binary_sensor.d", monitor.CategoryDoor), rule: NewRule{Entity: "binary_sensor.d", Kind: "between"}, want: ErrInvalidKind, outcome: audit.OutcomeError},
		{name: "malformed entity", setup: func(*fixture) {}, rule: NewRule{Entity: "SECRET ENTITY!", Kind: KindState}, want: ErrInvalidEntity, outcome: audit.OutcomeError},
		{name: "not shared", setup: func(f *fixture) { f.monitored("binary_sensor.d", monitor.CategoryDoor, "off") }, rule: NewRule{Entity: "binary_sensor.d", Kind: KindState}, want: ErrNotShared, outcome: audit.OutcomeDenied},
		{name: "shared but not monitored", setup: func(f *fixture) { f.share("binary_sensor.d", entities.AccessRead) }, rule: NewRule{Entity: "binary_sensor.d", Kind: KindState}, want: ErrNotMonitored, outcome: audit.OutcomeError},
		{name: "control access is enough", setup: func(f *fixture) {
			f.share("binary_sensor.d", entities.AccessControl)
			f.monitored("binary_sensor.d", monitor.CategoryDoor, "off")
		}, rule: NewRule{Entity: "binary_sensor.d", Kind: KindState}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.setup(f)
			rule, err := f.svc.AddRule(ctx, admin, tc.rule)
			entry := f.lastAudit()
			if tc.want == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if f.count("notification_rules") != 1 || rule.ID == 0 || !rule.Enabled || rule.CreatedBy != admin.UserID {
					t.Errorf("rule = %+v", rule)
				}
				if entry.Outcome != audit.OutcomeSuccess || entry.Command != "notifications.rule.add" {
					t.Errorf("audit = %+v", entry)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if f.count("notification_rules") != 0 {
				t.Error("a refused rule must not be stored")
			}
			if entry.Outcome != tc.outcome || entry.Detail != sanitized(tc.want) {
				t.Errorf("audit = %+v, want outcome %s detail %q", entry, tc.outcome, sanitized(tc.want))
			}
		})
	}
}

func ready(id string, cat monitor.Category) func(*fixture) {
	return func(f *fixture) { f.ready(id, cat) }
}

func TestService_addRule_storesWhatWasRequested_andRejectsADuplicate(t *testing.T) {
	f := newFixture(t)
	f.ready("sensor.t", monitor.CategoryTemperature)
	if _, err := f.svc.AddRule(ctx, admin, NewRule{Entity: "sensor.t", Kind: KindAbove, Threshold: fl(28.5)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AddRule(ctx, admin, NewRule{Entity: "sensor.t", Kind: KindBelow, Threshold: fl(5), Cooldown: dur(0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AddRule(ctx, admin, NewRule{Entity: "sensor.t", Kind: KindAbove, Threshold: fl(30)}); !errors.Is(err, ErrRuleExists) {
		t.Errorf("a second above rule for one sensor: %v", err)
	}
	views, err := f.svc.Rules(ctx, admin)
	if err != nil || len(views) != 2 {
		t.Fatalf("rules = %+v, %v", views, err)
	}
	above, below := views[0], views[1]
	if above.Threshold == nil || *above.Threshold != 28.5 || above.Cooldown != nil {
		t.Errorf("a nil cooldown means the category default: %+v", above.Rule)
	}
	if below.Cooldown == nil || *below.Cooldown != 0 {
		t.Errorf("an explicit zero cooldown means no cooldown: %+v", below.Rule)
	}
	if above.Category != monitor.CategoryTemperature || above.Status != StatusActive || above.UpdatedBy != admin.UserID {
		t.Errorf("view = %+v", above)
	}
}

func TestService_rulesStayDormantWhenAnEntityIsRevokedRemovedOrReclassified_andNeverAreDeleted(t *testing.T) {
	f := newFixture(t)
	f.ready("binary_sensor.door", monitor.CategoryDoor)
	rule, err := f.svc.AddRule(ctx, admin, NewRule{Entity: "binary_sensor.door", Kind: KindState})
	if err != nil {
		t.Fatal(err)
	}
	status := func() RuleStatus {
		t.Helper()
		views, err := f.svc.Rules(ctx, admin)
		if err != nil || len(views) != 1 {
			t.Fatalf("rules = %+v, %v (the rule must never be deleted)", views, err)
		}
		return views[0].Status
	}
	expect := func(step string, want RuleStatus) {
		t.Helper()
		if got := status(); got != want {
			t.Errorf("%s: status = %s, want %s", step, got, want)
		}
		if f.count("notification_rules") != 1 {
			t.Errorf("%s: the rule must still be stored", step)
		}
	}
	expect("added", StatusActive)

	if err := f.svc.SetRuleEnabled(ctx, admin, rule.ID, false); err != nil {
		t.Fatal(err)
	}
	expect("paused", StatusPaused)
	if err := f.svc.SetRuleEnabled(ctx, admin, rule.ID, true); err != nil {
		t.Fatal(err)
	}
	expect("resumed", StatusActive)

	if _, err := f.perms.Revoke(ctx, "binary_sensor.door"); err != nil {
		t.Fatal(err)
	}
	expect("revoked", StatusNeedsSharing)
	f.share("binary_sensor.door", entities.AccessRead)
	expect("shared again", StatusActive)

	f.tracker.Apply(monitor.Update{ID: "binary_sensor.door", Removed: true}, f.now)
	expect("removed from Home Assistant", StatusNotMonitored)
	f.monitored("binary_sensor.door", monitor.CategoryDoor, "off")
	expect("back", StatusActive)

	f.monitored("binary_sensor.door", monitor.CategoryTemperature, "21")
	expect("reclassified as numeric", StatusIncompatible)
}

func TestService_pauseResumeRemove_andUnknownRules(t *testing.T) {
	f := newFixture(t)
	f.ready("binary_sensor.door", monitor.CategoryDoor)
	rule, _ := f.svc.AddRule(ctx, admin, NewRule{Entity: "binary_sensor.door", Kind: KindState, Cooldown: dur(time.Minute)})

	if err := f.svc.SetRuleEnabled(ctx, admin, rule.ID, false); err != nil {
		t.Fatal(err)
	}
	if e := f.lastAudit(); e.Command != "notifications.rule.pause" || e.Outcome != audit.OutcomeSuccess || e.EntityID != "binary_sensor.door" || e.Detail != "rule=1 kind=state cooldown=1m0s" {
		t.Errorf("pause audit = %+v", e)
	}
	if v, _ := f.svc.Rules(ctx, admin); v[0].Enabled {
		t.Error("paused")
	}
	if err := f.svc.SetRuleEnabled(ctx, admin, rule.ID, true); err != nil {
		t.Fatal(err)
	}
	if e := f.lastAudit(); e.Command != "notifications.rule.resume" {
		t.Errorf("resume audit = %+v", e)
	}

	for name, call := range map[string]func() error{
		"pause":  func() error { return f.svc.SetRuleEnabled(ctx, admin, 9999, false) },
		"resume": func() error { return f.svc.SetRuleEnabled(ctx, admin, 9999, true) },
		"remove": func() error { return f.svc.RemoveRule(ctx, admin, 9999) },
	} {
		if err := call(); !errors.Is(err, ErrRuleNotFound) {
			t.Errorf("%s of an unknown rule: %v", name, err)
		}
		if e := f.lastAudit(); e.Outcome != audit.OutcomeError || e.Detail != ErrRuleNotFound.Error() {
			t.Errorf("%s audit = %+v", name, e)
		}
	}

	if err := f.svc.RemoveRule(ctx, admin, rule.ID); err != nil {
		t.Fatal(err)
	}
	if f.count("notification_rules") != 0 {
		t.Error("removed")
	}
	if e := f.lastAudit(); e.Command != "notifications.rule.remove" || e.EntityID != "binary_sensor.door" || e.Outcome != audit.OutcomeSuccess {
		t.Errorf("remove audit = %+v", e)
	}
}

func TestService_auditNeverContainsSensorValuesOrRawInput(t *testing.T) {
	f := newFixture(t)
	f.share("binary_sensor.door", entities.AccessRead)
	f.monitored("binary_sensor.door", monitor.CategoryDoor, "RAWVALUE-9876")
	_ = f.svc.SetChannel(ctx, admin, "channel-SECRET-xyz")
	_ = f.svc.SetChannel(ctx, admin, testChannel)
	rule, _ := f.svc.AddRule(ctx, admin, NewRule{Entity: "binary_sensor.door", Kind: KindState})
	_, _ = f.svc.AddRule(ctx, admin, NewRule{Entity: "ENTITY-SECRET!!", Kind: KindState})
	_, _ = f.svc.AddRule(ctx, admin, NewRule{Entity: "binary_sensor.door", Kind: "kind-SECRET"})
	_ = f.svc.SetRuleEnabled(ctx, admin, rule.ID, false)
	_ = f.svc.RemoveRule(ctx, admin, rule.ID)
	var all strings.Builder
	for _, e := range f.auditRows() {
		fmt.Fprintf(&all, "%+v\n", e)
	}
	for _, secret := range []string{"RAWVALUE-9876", "channel-SECRET", "ENTITY-SECRET", "kind-SECRET"} {
		if strings.Contains(all.String(), secret) {
			t.Errorf("the audit log contains %q:\n%s", secret, all.String())
		}
	}
}

// If the audit entry cannot be written, no change may be committed.
func TestService_auditFailureRollsEveryChangeBack(t *testing.T) {
	f := newFixture(t)
	f.ready("binary_sensor.door", monitor.CategoryDoor)
	f.ready("binary_sensor.window", monitor.CategoryWindow)
	if err := f.svc.SetChannel(ctx, admin, testChannel); err != nil {
		t.Fatal(err)
	}
	rule, err := f.svc.AddRule(ctx, admin, NewRule{Entity: "binary_sensor.door", Kind: KindState})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.db.Exec("CREATE TRIGGER audit_down BEFORE INSERT ON command_audit_logs BEGIN SELECT RAISE(ABORT, 'audit down'); END"); err != nil {
		t.Fatal(err)
	}
	other := "300000000000000003"
	mutations := map[string]func() error{
		"SetChannel": func() error { return f.svc.SetChannel(ctx, admin, other) },
		"SetEnabled": func() error { return f.svc.SetEnabled(ctx, admin, false) },
		"AddRule": func() error {
			_, e := f.svc.AddRule(ctx, admin, NewRule{Entity: "binary_sensor.window", Kind: KindState})
			return e
		},
		"SetRuleEnabled": func() error { return f.svc.SetRuleEnabled(ctx, admin, rule.ID, false) },
		"RemoveRule":     func() error { return f.svc.RemoveRule(ctx, admin, rule.ID) },
	}
	for name, call := range mutations {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "audit") {
			t.Errorf("%s must fail when its audit entry cannot be written: %v", name, err)
		}
	}
	cfg, _ := f.svc.Config(ctx, admin)
	views, _ := f.svc.Rules(ctx, admin)
	if cfg.Settings.ChannelID != testChannel || !cfg.Settings.Enabled || cfg.Rules != 1 || len(views) != 1 || !views[0].Enabled || views[0].ID != rule.ID {
		t.Fatalf("a change survived although its audit entry failed: %+v %+v", cfg, views)
	}
	// A refused attempt whose audit also fails still reports the real reason.
	if err := f.svc.SetEnabled(ctx, user, false); !errors.Is(err, auth.ErrAdminRequired) {
		t.Errorf("the original refusal must be returned: %v", err)
	}

	if _, err := f.db.Exec("DROP TRIGGER audit_down"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetChannel(ctx, admin, other); err != nil {
		t.Fatalf("with the audit log back the same change succeeds: %v", err)
	}
}

func TestService_concurrentAddRule_atTheLimit_admitsExactlyTheRemainingSlots(t *testing.T) {
	f := newFixture(t)
	const free, contenders = 10, 40
	for i := 0; i < MaxRules-free; i++ {
		if _, err := f.store.InsertRule(ctx, Rule{Entity: entities.ID(fmt.Sprintf("binary_sensor.fill%03d", i)), Kind: KindState, Enabled: true,
			CreatedBy: "a", CreatedAt: f.now, UpdatedBy: "a", UpdatedAt: f.now}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < contenders; i++ {
		f.ready(fmt.Sprintf("binary_sensor.race%02d", i), monitor.CategoryDoor)
	}

	start := make(chan struct{})
	results := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := f.svc.AddRule(ctx, admin, NewRule{Entity: entities.ID(fmt.Sprintf("binary_sensor.race%02d", i)), Kind: KindState})
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	var ok, limited, other int
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrRuleLimit):
			limited++
		default:
			other++
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != free || limited != contenders-free || other != 0 {
		t.Fatalf("admitted %d, limited %d, other %d; want %d, %d, 0", ok, limited, other, free, contenders-free)
	}
	if n := f.count("notification_rules"); n != MaxRules {
		t.Errorf("rules = %d, want exactly %d", n, MaxRules)
	}
	var success, refused int
	for _, e := range f.auditRows() {
		switch e.Outcome {
		case audit.OutcomeSuccess:
			success++
		case audit.OutcomeError:
			if e.Detail == ErrRuleLimit.Error() {
				refused++
			}
		}
	}
	if success != free || refused != contenders-free {
		t.Errorf("audit: %d successes and %d limit refusals, want %d and %d", success, refused, free, contenders-free)
	}
}
