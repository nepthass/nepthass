package notify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
)

// auditTimeout bounds the best-effort audit write of a refused attempt, which
// must not be cut short by the caller's cancellation.
const auditTimeout = 5 * time.Second

// SensorLookup tells whether a sensor is currently monitored. *monitor.Tracker
// satisfies it. It is empty until the monitor has synchronized.
type SensorLookup interface {
	Get(id entities.ID) (monitor.Sensor, bool)
}

// Deps are the collaborators of Service.
type Deps struct {
	// GuildID is the one configured guild. Settings are keyed by it and callers
	// cannot name another guild.
	GuildID     string
	DB          *sql.DB
	Store       *Store
	Permissions *entities.Permissions
	Sensors     SensorLookup
	Audit       *audit.Log
	Now         func() time.Time
	Logger      *slog.Logger
}

// Service is the administrator-only configuration service. It is safe for
// concurrent use.
type Service struct{ d Deps }

// NewService returns a Service; Now and Logger default to time.Now and a discard logger.
func NewService(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{d: d}
}

// txn bundles the transaction-scoped collaborators handed to a mutation.
type txn struct {
	store *Store
	perms *entities.Permissions
}

// mutate runs fn and the audit entry for it in ONE SQLite transaction, following
// the atomic pattern of setup.applyTx (and not entities.Service.audited, which
// audits after the fact): if the audit row cannot be written, nothing is
// committed. fn may fill e.EntityID, e.Action and e.Detail. A refused or failed
// attempt rolls back first (the immediate transaction holds the write lock) and
// is then audited best-effort with a sanitized reason.
func (s *Service) mutate(ctx context.Context, actor auth.Actor, e audit.Entry, fn func(t txn, e *audit.Entry) error) error {
	e.Time, e.ActorID = s.d.Now(), actor.UserID
	if err := actor.Require(auth.LevelAdmin); err != nil {
		s.auditRefused(ctx, e, err)
		return err
	}
	tx, err := s.d.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin notification change: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	if err := fn(txn{store: s.d.Store.WithTx(tx), perms: s.d.Permissions.WithTx(tx)}, &e); err != nil {
		_ = tx.Rollback() // release the write lock before the refusal is audited
		s.auditRefused(ctx, e, err)
		return err
	}
	e.Outcome = audit.OutcomeSuccess
	if err := s.d.Audit.WithTx(tx).Record(ctx, e); err != nil {
		return fmt.Errorf("the change was not saved because its audit entry could not be written: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit notification change: %w", err)
	}
	return nil
}

// ErrChannelRejected and ErrTestFailed are fixed sentinels for administrative
// actions that are refused before (or without) a stored change; their text is
// all the audit log ever receives about them.
var (
	ErrChannelRejected = errors.New("channel rejected")
	ErrTestFailed      = errors.New("test message not delivered")
)

// Record audits an administrative action that is not a stored change: a denied
// attempt, a rejected channel, a test message. cause maps to a fixed sentinel
// message (see sanitized), never to caller text; nil records a success. The
// write is best effort and bounded, like the audit of a refused mutation.
func (s *Service) Record(ctx context.Context, actor auth.Actor, command, action string, cause error) {
	e := audit.Entry{Time: s.d.Now(), ActorID: actor.UserID, Command: command, Action: action}
	if cause != nil {
		s.auditRefused(ctx, e, cause)
		return
	}
	e.Outcome = audit.OutcomeSuccess
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	if err := s.d.Audit.Record(actx, e); err != nil {
		s.d.Logger.Error("audit write failed", "command", e.Command, "error", err)
	}
}

// auditRefused records a denied or failed attempt, outside any transaction.
func (s *Service) auditRefused(ctx context.Context, e audit.Entry, cause error) {
	e.Outcome = audit.OutcomeError
	if isDenial(cause) {
		e.Outcome = audit.OutcomeDenied
	}
	e.Detail = sanitized(cause)
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	if err := s.d.Audit.Record(actx, e); err != nil {
		s.d.Logger.Error("audit write failed", "command", e.Command, "error", err)
	}
}

func isDenial(err error) bool {
	return errors.Is(err, auth.ErrAdminRequired) || errors.Is(err, auth.ErrNotAuthorized) || errors.Is(err, entities.ErrAccessDenied)
}

// known lists the errors whose own message is safe to store. Anything else (a
// database error, a wrapped message) is stored as "internal error", so the audit
// log never receives text that could carry a sensor value, a path or a secret.
var known = []error{
	ErrChannelRejected, ErrTestFailed,
	auth.ErrAdminRequired, auth.ErrNotAuthorized, ErrNotConfigured, ErrInvalidChannel, ErrInvalidEntity, ErrInvalidKind,
	ErrInvalidThreshold, ErrInvalidCooldown, ErrNotShared, ErrNotMonitored, ErrIncompatible, ErrRuleLimit, ErrRuleExists, ErrRuleNotFound,
	ErrInvalidDuration, ErrDurationNotAllowed, ErrInvalidClear, ErrClearNotAllowed, ErrHazardImmediate, ErrUnitUnsupported, ErrUnitBound,
}

func sanitized(err error) string {
	for _, k := range known {
		if errors.Is(err, k) {
			return k.Error()
		}
	}
	return "internal error"
}

// Config returns the current configuration. Reading it requires an administrator
// (rules name entities) but is not audited.
func (s *Service) Config(ctx context.Context, actor auth.Actor) (Config, error) {
	if err := actor.Require(auth.LevelAdmin); err != nil {
		return Config{}, err
	}
	st, ok, err := s.d.Store.GetSettings(ctx, s.d.GuildID)
	if err != nil {
		return Config{}, err
	}
	n, err := s.d.Store.CountRules(ctx)
	if err != nil {
		return Config{}, err
	}
	return Config{Configured: ok, Settings: st, Rules: n}, nil
}

// SetChannel sets or changes the guild's alert channel. The first call creates
// the settings with notifications enabled. Only the shape of the ID is checked
// here: whether the channel exists, is in this guild and lets the bot post is for
// the Discord adapter to verify.
func (s *Service) SetChannel(ctx context.Context, actor auth.Actor, channelID string) error {
	return s.mutate(ctx, actor, audit.Entry{Command: "notifications.channel", Action: "set"}, func(t txn, e *audit.Entry) error {
		if !validSnowflake(channelID) {
			return ErrInvalidChannel
		}
		e.Detail = "channel=" + channelID
		return t.store.UpsertChannel(ctx, s.d.GuildID, channelID, actor.UserID, s.d.Now())
	})
}

// SetEnabled turns notifications on or off without losing the configuration.
func (s *Service) SetEnabled(ctx context.Context, actor auth.Actor, enabled bool) error {
	action := "disable"
	if enabled {
		action = "enable"
	}
	return s.mutate(ctx, actor, audit.Entry{Command: "notifications.enable", Action: action}, func(t txn, e *audit.Entry) error {
		ok, err := t.store.SetEnabled(ctx, s.d.GuildID, enabled, actor.UserID, s.d.Now())
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotConfigured
		}
		return nil
	})
}

// AddRule adds a rule. The entity must be shared with at least read access and
// currently monitored, and the kind must fit its category.
func (s *Service) AddRule(ctx context.Context, actor auth.Actor, n NewRule) (Rule, error) {
	e := audit.Entry{Command: "notifications.rule.add", Action: "invalid"}
	if n.Kind.Valid() {
		e.Action = string(n.Kind)
	}
	if id, err := entities.ParseID(string(n.Entity)); err == nil {
		e.EntityID = string(id)
	}
	var added Rule
	err := s.mutate(ctx, actor, e, func(t txn, e *audit.Entry) error {
		if _, err := entities.ParseID(string(n.Entity)); err != nil {
			return ErrInvalidEntity
		}
		if err := validateRule(n); err != nil {
			return err
		}
		access, err := t.perms.Get(ctx, n.Entity)
		if err != nil {
			return err
		}
		if !access.AtLeast(entities.AccessRead) {
			return ErrNotShared
		}
		sensor, ok := s.d.Sensors.Get(n.Entity)
		if !ok {
			return ErrNotMonitored
		}
		if !compatible(n.Kind, sensor.Category) {
			return ErrIncompatible
		}
		spec, _ := monitor.SpecFor(sensor.Category) // compatible() guarantees the row
		if err := checkCapabilities(n, spec); err != nil {
			return err
		}
		unit := ""
		if n.Kind != KindState {
			if !spec.SupportsUnit(sensor.Unit) {
				return ErrUnitUnsupported
			}
			unit = sensor.Unit
		}
		count, err := t.store.CountRules(ctx)
		if err != nil {
			return err
		}
		if count >= MaxRules {
			return ErrRuleLimit
		}
		now := s.d.Now()
		r := Rule{Entity: n.Entity, Kind: n.Kind, Threshold: n.Threshold, Cooldown: n.Cooldown, Duration: n.Duration, Clear: n.Clear, Unit: unit, Enabled: true,
			CreatedBy: actor.UserID, CreatedAt: now, UpdatedBy: actor.UserID, UpdatedAt: now}
		if r.ID, err = t.store.InsertRule(ctx, r); err != nil {
			return err
		}
		added = r
		e.Detail = ruleDetail(r)
		return nil
	})
	if err != nil {
		return Rule{}, err
	}
	return added, nil
}

// Rules lists every rule with its current status. Reading requires an
// administrator and is not audited.
func (s *Service) Rules(ctx context.Context, actor auth.Actor) ([]RuleView, error) {
	if err := actor.Require(auth.LevelAdmin); err != nil {
		return nil, err
	}
	rules, err := s.d.Store.ListRules(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]RuleView, 0, len(rules))
	for _, r := range rules {
		access, err := s.d.Permissions.Get(ctx, r.Entity)
		if err != nil {
			return nil, err
		}
		sensor, monitored := s.d.Sensors.Get(r.Entity)
		v := RuleView{Rule: r, Status: statusOf(r, access, sensor, monitored)}
		if monitored {
			v.Category = sensor.Category
		}
		views = append(views, v)
	}
	return views, nil
}

// SetRuleEnabled pauses or resumes a rule.
func (s *Service) SetRuleEnabled(ctx context.Context, actor auth.Actor, id int64, enabled bool) error {
	action := "pause"
	if enabled {
		action = "resume"
	}
	return s.mutate(ctx, actor, audit.Entry{Command: "notifications.rule." + action, Action: action}, func(t txn, e *audit.Entry) error {
		r, ok, err := t.store.GetRule(ctx, id)
		if err != nil {
			return err
		}
		if !ok {
			return ErrRuleNotFound
		}
		e.EntityID, e.Detail = string(r.Entity), ruleDetail(r)
		if _, err := t.store.SetRuleEnabled(ctx, id, enabled, actor.UserID, s.d.Now()); err != nil {
			return err
		}
		return nil
	})
}

// RemoveRule deletes a rule.
func (s *Service) RemoveRule(ctx context.Context, actor auth.Actor, id int64) error {
	return s.mutate(ctx, actor, audit.Entry{Command: "notifications.rule.remove", Action: "remove"}, func(t txn, e *audit.Entry) error {
		r, ok, err := t.store.GetRule(ctx, id)
		if err != nil {
			return err
		}
		if !ok {
			return ErrRuleNotFound
		}
		e.EntityID, e.Detail = string(r.Entity), ruleDetail(r)
		_, err = t.store.DeleteRule(ctx, id)
		return err
	})
}

// ConfirmUnit binds a numeric rule that predates unit binding to the sensor's
// CURRENT unit, on an administrator's say-so that its threshold is expressed in
// that unit. It is the recovery path for a rule NeptHass could not bind by itself
// (a class with several units, such as temperature). It refuses a rule that is
// already bound (a unit is never rebound), a sensor that is not monitored, and a
// unit that is missing or not supported for the sensor type.
func (s *Service) ConfirmUnit(ctx context.Context, actor auth.Actor, id int64) (string, error) {
	var bound string
	err := s.mutate(ctx, actor, audit.Entry{Command: "notifications.rule.unit", Action: "confirm"}, func(t txn, e *audit.Entry) error {
		r, ok, err := t.store.GetRule(ctx, id)
		if err != nil {
			return err
		}
		if !ok {
			return ErrRuleNotFound
		}
		e.EntityID = string(r.Entity)
		if r.Kind == KindState {
			return ErrIncompatible
		}
		if r.Unit != "" {
			return ErrUnitBound
		}
		sensor, monitored := s.d.Sensors.Get(r.Entity)
		if !monitored {
			return ErrNotMonitored
		}
		spec, found := monitor.SpecFor(sensor.Category)
		if !found || !compatible(r.Kind, sensor.Category) {
			return ErrIncompatible
		}
		if !spec.SupportsUnit(sensor.Unit) {
			return ErrUnitUnsupported
		}
		changed, err := t.store.SetRuleUnit(ctx, id, sensor.Unit, actor.UserID, s.d.Now())
		if err != nil {
			return err
		}
		if !changed {
			return ErrUnitBound
		}
		bound = sensor.Unit
		e.Detail = fmt.Sprintf("rule=%d unit=%s", id, sensor.Unit)
		return nil
	})
	return bound, err
}

// BindLegacyUnits is the one-time, audited binding of rules created before unit
// binding. A rule is bound ONLY when the mapping is unambiguous: a numeric rule
// with no unit, whose sensor is monitored and reporting a real value, in a class
// with exactly one documented unit (humidity and battery: %), and whose current
// unit is that unit. It never converts a value, never guesses between units
// (temperature may be °C, °F or K, so an administrator must confirm it), and
// leaves everything else untouched. Each binding is one transaction with its
// audit entry, written by SystemActor, and a rule is bound at most once because
// SetRuleUnit changes only an empty unit. It returns how many rules it bound.
func (s *Service) BindLegacyUnits(ctx context.Context) (int, error) {
	rules, err := s.d.Store.ListRules(ctx)
	if err != nil {
		return 0, err
	}
	bound := 0
	for _, r := range rules {
		if r.Kind == KindState || r.Unit != "" {
			continue
		}
		sensor, monitored := s.d.Sensors.Get(r.Entity)
		if !monitored || !reliable(sensor) || !compatible(r.Kind, sensor.Category) {
			continue
		}
		spec, _ := monitor.SpecFor(sensor.Category)
		unit, single := spec.SingleUnit()
		if !single || sensor.Unit != unit {
			continue
		}
		ok, err := s.bindInTx(ctx, r, unit)
		if err != nil {
			return bound, err
		}
		if ok {
			bound++
		}
	}
	return bound, nil
}

func (s *Service) bindInTx(ctx context.Context, r Rule, unit string) (bool, error) {
	tx, err := s.d.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin unit binding: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := s.d.Now()
	changed, err := s.d.Store.WithTx(tx).SetRuleUnit(ctx, r.ID, unit, SystemActor, now)
	if err != nil || !changed {
		return false, err
	}
	e := audit.Entry{Time: now, ActorID: SystemActor, Command: "notifications.rule.unit", EntityID: string(r.Entity), Action: "bind",
		Outcome: audit.OutcomeSuccess, Detail: fmt.Sprintf("rule=%d unit=%s", r.ID, unit)}
	if err := s.d.Audit.WithTx(tx).Record(ctx, e); err != nil {
		return false, fmt.Errorf("the unit was not bound because its audit entry could not be written: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit unit binding: %w", err)
	}
	return true, nil
}

func validateRule(n NewRule) error {
	if !n.Kind.Valid() {
		return ErrInvalidKind
	}
	switch n.Kind {
	case KindState:
		if n.Threshold != nil {
			return fmt.Errorf("%w: a state rule takes no threshold", ErrInvalidThreshold)
		}
	default:
		if n.Threshold == nil {
			return fmt.Errorf("%w: a threshold is required", ErrInvalidThreshold)
		}
		if v := *n.Threshold; math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > MaxThreshold {
			return ErrInvalidThreshold
		}
	}
	if n.Cooldown != nil {
		if d := *n.Cooldown; d < 0 || d > MaxCooldown || d%time.Second != 0 {
			return ErrInvalidCooldown
		}
	}
	if n.Duration != 0 && (n.Duration < MinDuration || n.Duration > MaxDuration || n.Duration%time.Second != 0) {
		return ErrInvalidDuration
	}
	if n.Clear != nil {
		if n.Kind == KindState {
			return ErrClearNotAllowed
		}
		v := *n.Clear
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > MaxThreshold {
			return ErrInvalidClear
		}
		// The clear value is on the recovery side of the threshold: below it for an
		// "above" rule, above it for a "below" rule. Equal is refused: the rule could
		// never leave its alert state without crossing the trigger itself.
		if (n.Kind == KindAbove && !(v < *n.Threshold)) || (n.Kind == KindBelow && !(v > *n.Threshold)) {
			return ErrInvalidClear
		}
	}
	return nil
}

// checkCapabilities refuses a duration or a clear value the sensor class does not
// support. A hazard (the water leak) is immediate only: that is a product
// decision, not a limitation, so it has its own message.
func checkCapabilities(n NewRule, spec monitor.Spec) error {
	if n.Duration > 0 || n.Clear != nil {
		if spec.Hazard {
			return ErrHazardImmediate
		}
	}
	if n.Duration > 0 {
		need := monitor.CapStateDuration
		if n.Kind != KindState {
			need = monitor.CapThresholdDuration
		}
		if !spec.Caps.Has(need) {
			return ErrDurationNotAllowed
		}
	}
	if n.Clear != nil && !spec.Caps.Has(monitor.CapHysteresis) {
		return ErrClearNotAllowed
	}
	return nil
}

// compatible reports whether a rule kind fits a sensor category, from the
// category's declared capabilities: state rules for binary classes, above and
// below for numeric ones. An unknown category fits nothing.
func compatible(k Kind, c monitor.Category) bool {
	s, ok := monitor.SpecFor(c)
	if !ok {
		return false
	}
	if k == KindState {
		return s.Caps.Has(monitor.CapState)
	}
	return s.Caps.Has(monitor.CapThreshold)
}

func statusOf(r Rule, access entities.Access, sensor monitor.Sensor, monitored bool) RuleStatus {
	switch {
	case !r.Enabled:
		return StatusPaused
	case !access.AtLeast(entities.AccessRead):
		return StatusNeedsSharing
	case !monitored:
		return StatusNotMonitored
	case !compatible(r.Kind, sensor.Category):
		return StatusIncompatible
	case r.Kind != KindState && r.Unit == "":
		return StatusUnitUnresolved
	case r.Kind != KindState && sensor.Unit != r.Unit:
		return StatusUnitChanged
	}
	return StatusActive
}

// ruleDetail is the audit text of a rule: its configuration, never a sensor value.
func ruleDetail(r Rule) string {
	var b strings.Builder
	fmt.Fprintf(&b, "rule=%d kind=%s", r.ID, r.Kind)
	if r.Threshold != nil {
		b.WriteString(" threshold=" + strconv.FormatFloat(*r.Threshold, 'g', -1, 64))
	}
	if r.Clear != nil {
		b.WriteString(" clear=" + strconv.FormatFloat(*r.Clear, 'g', -1, 64))
	}
	if r.Duration > 0 {
		b.WriteString(" for=" + r.Duration.String())
	}
	if r.Unit != "" {
		b.WriteString(" unit=" + r.Unit) // only a registry-supported unit is ever bound
	}
	if r.Cooldown == nil {
		b.WriteString(" cooldown=default")
	} else {
		b.WriteString(" cooldown=" + r.Cooldown.String())
	}
	return b.String()
}

func validSnowflake(s string) bool {
	if len(s) < 5 || len(s) > 32 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
