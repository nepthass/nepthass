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
	auth.ErrAdminRequired, auth.ErrNotAuthorized, ErrNotConfigured, ErrInvalidChannel, ErrInvalidEntity, ErrInvalidKind,
	ErrInvalidThreshold, ErrInvalidCooldown, ErrNotShared, ErrNotMonitored, ErrIncompatible, ErrRuleLimit, ErrRuleExists, ErrRuleNotFound,
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
		count, err := t.store.CountRules(ctx)
		if err != nil {
			return err
		}
		if count >= MaxRules {
			return ErrRuleLimit
		}
		now := s.d.Now()
		r := Rule{Entity: n.Entity, Kind: n.Kind, Threshold: n.Threshold, Cooldown: n.Cooldown, Enabled: true,
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
	return nil
}

// compatible reports whether a rule kind fits a sensor category: state rules for
// binary sensors, above and below for numeric ones.
func compatible(k Kind, c monitor.Category) bool {
	if k == KindState {
		return !c.Numeric()
	}
	return c.Numeric()
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
