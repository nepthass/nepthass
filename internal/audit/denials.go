package audit

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Reason is why an administrative command was refused. The vocabulary is fixed:
// nothing a caller types ever becomes a reason.
type Reason string

const (
	// ReasonNotAdmin: a user on the allowlist who is not an administrator.
	ReasonNotAdmin Reason = "not_admin"
	// ReasonNotAllowed: a user in the configured guild who is on no allowlist.
	ReasonNotAllowed Reason = "not_allowed"
	// ReasonWrongGuild: the interaction came from another guild or from a direct message.
	ReasonWrongGuild Reason = "wrong_guild"
)

// Limits that keep the denial audit from exhausting SQLite or hiding real events.
const (
	// DenialBurst and DenialRefill: per actor, a burst of rows then one per refill.
	DenialBurst  = 3
	DenialRefill = time.Minute
	// DenialsPerHour caps the rows written by all actors together.
	DenialsPerHour = 60
	// MaxDeniedRows is how many denial rows are kept (the newest); the retention
	// task deletes the oldest beyond it, in addition to the normal retention period.
	MaxDeniedRows = 5000

	maxActors = 2048
	maxSeen   = 256
)

// DenialAttempt describes one refused administrative command. Every string is
// untrusted: Record validates it and stores only what passes.
type DenialAttempt struct {
	// InteractionID identifies the Discord interaction; a replay of the same one is recorded once.
	InteractionID string
	ActorID       string
	// Command and Action are the command and subcommand NAMES, never option values.
	Command, Action string
	GuildID         string
	Reason          Reason
}

// Denials records refused administrative commands as audit rows (outcome denied).
//
// It exists to answer "who tried to use an administrator command", so it records
// the actor, the command and subcommand names and a fixed reason, and nothing the
// caller supplied beyond that: no option value (an option may carry an entity ID
// or free text), no entity name, no Discord error text. It is bounded three ways:
// a burst-limited bucket per actor, a global cap per hour, and a cap on retained
// rows. Rows it drops are counted and folded into the next row as suppressed=N.
//
// A nil *Denials records nothing. Record never returns an error and never panics
// on bad input: a failed audit write is logged and has no effect on the decision
// that was already made, so it can never grant access.
type Denials struct {
	log    *Log
	logger *slog.Logger
	now    func() time.Time

	mu         sync.Mutex
	actors     map[string]*bucket
	hourStart  time.Time
	hourCount  int
	seen       map[string]struct{}
	ring       []string
	ringPos    int
	suppressed int
}

type bucket struct {
	tokens float64
	at     time.Time
}

// NewDenials returns a recorder that writes to log.
func NewDenials(log *Log, logger *slog.Logger, now func() time.Time) *Denials {
	if now == nil {
		now = time.Now
	}
	return &Denials{log: log, logger: logger, now: now, actors: map[string]*bucket{}, seen: map[string]struct{}{}}
}

// Record writes one denial row unless it is a replay or a limit is reached.
func (d *Denials) Record(ctx context.Context, a DenialAttempt) {
	if d == nil || d.log == nil {
		return
	}
	actor, guild := snowflakeOr(a.ActorID, "unknown"), snowflakeOr(a.GuildID, "none")
	command, action := word(a.Command), word(a.Action)
	if command == "" || !validReason(a.Reason) {
		return // nothing safe to say about it
	}
	now := d.now()

	d.mu.Lock()
	if a.InteractionID != "" {
		if _, replay := d.seen[a.InteractionID]; replay {
			d.mu.Unlock()
			return
		}
		d.remember(a.InteractionID)
	}
	b := d.actors[actor]
	if b == nil {
		if len(d.actors) >= maxActors {
			clear(d.actors) // bounded memory; the global cap still bounds the rows
		}
		b = &bucket{tokens: DenialBurst, at: now}
		d.actors[actor] = b
	}
	b.tokens = min(DenialBurst, b.tokens+float64(now.Sub(b.at))/float64(DenialRefill))
	b.at = now
	if now.Sub(d.hourStart) >= time.Hour || now.Before(d.hourStart) {
		d.hourStart, d.hourCount = now, 0
	}
	if b.tokens < 1 || d.hourCount >= DenialsPerHour {
		d.suppressed++
		d.mu.Unlock()
		return
	}
	b.tokens--
	d.hourCount++
	dropped := d.suppressed
	d.suppressed = 0
	d.mu.Unlock()

	detail := "reason=" + string(a.Reason)
	if a.Reason == ReasonWrongGuild {
		detail += " guild=" + guild
	}
	if dropped > 0 {
		detail += fmt.Sprintf(" suppressed=%d", dropped)
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	e := Entry{Time: now, ActorID: actor, Command: command, Action: action, Outcome: OutcomeDenied, Detail: detail}
	if err := d.log.Record(wctx, e); err != nil && d.logger != nil {
		d.logger.Error("could not audit a refused command", "command", command, "error", err)
	}
}

func (d *Denials) remember(id string) {
	if len(d.ring) < maxSeen {
		d.ring = append(d.ring, id)
	} else {
		delete(d.seen, d.ring[d.ringPos])
		d.ring[d.ringPos] = id
		d.ringPos = (d.ringPos + 1) % maxSeen
	}
	d.seen[id] = struct{}{}
}

func validReason(r Reason) bool {
	return r == ReasonNotAdmin || r == ReasonNotAllowed || r == ReasonWrongGuild
}

// snowflakeOr returns s when it is a Discord ID (1 to 20 digits), else fallback.
func snowflakeOr(s, fallback string) string {
	if len(s) == 0 || len(s) > 20 {
		return fallback
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return fallback
		}
	}
	return s
}

// word returns s when it is a command-style name (1 to 32 of a-z, 0-9, _, -),
// else the empty string. Command and subcommand names come from a fixed set, so
// anything else is not a name NeptHass defined and is not stored.
func word(s string) string {
	if len(s) == 0 || len(s) > 32 {
		return ""
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return ""
		}
	}
	return s
}

// PruneDenied keeps only the newest keep denial rows written by Denials (they all
// have a detail that starts with "reason=") and returns how many it removed. Other
// audit rows are never touched.
func (l *Log) PruneDenied(ctx context.Context, keep int) (int64, error) {
	res, err := l.q.ExecContext(ctx, `DELETE FROM command_audit_logs WHERE id IN (
SELECT id FROM command_audit_logs WHERE outcome = 'denied' AND detail LIKE 'reason=%' ORDER BY id DESC LIMIT -1 OFFSET ?)`, keep)
	if err != nil {
		return 0, fmt.Errorf("prune denied audit rows: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune denied audit rows: %w", err)
	}
	return n, nil
}
