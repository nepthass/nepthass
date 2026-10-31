// Package audit records security-relevant actions (device control attempts
// and permission changes) in the SQLite database.
package audit

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/nepthass/nepthass/internal/storage"
)

// Outcome is the result of an audited action.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeDenied  Outcome = "denied"
	OutcomeError   Outcome = "error"
)

const (
	maxDetailLen  = 300
	pruneInterval = 6 * time.Hour
)

// Entry is one audit record. EntityID may be empty for non-entity actions.
type Entry struct {
	Time     time.Time
	ActorID  string
	Command  string
	EntityID string
	Action   string
	Outcome  Outcome
	Detail   string
}

// queryer is satisfied by both *sql.DB and *sql.Tx.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Log stores and queries audit entries.
type Log struct{ q queryer }

// New returns an audit log backed by db.
func New(db *sql.DB) *Log { return &Log{q: db} }

// WithTx returns a log whose writes join tx, so an audit entry commits or rolls
// back together with the change it describes.
func (l *Log) WithTx(tx *sql.Tx) *Log { return &Log{q: tx} }

// Record appends an entry. Detail is truncated to keep rows small.
func (l *Log) Record(ctx context.Context, e Entry) error {
	detail := e.Detail
	if len(detail) > maxDetailLen {
		detail = detail[:maxDetailLen]
	}
	var entity any
	if e.EntityID != "" {
		entity = e.EntityID
	}
	_, err := l.q.ExecContext(ctx,
		"INSERT INTO command_audit_logs (occurred_at, actor_id, command, entity_id, action, outcome, detail) VALUES (?, ?, ?, ?, ?, ?, ?)",
		storage.FormatTime(e.Time), e.ActorID, e.Command, entity, e.Action, string(e.Outcome), detail)
	if err != nil {
		return fmt.Errorf("record audit entry: %w", err)
	}
	return nil
}

// Recent returns up to limit entries, newest first.
func (l *Log) Recent(ctx context.Context, limit int) ([]Entry, error) {
	rows, err := l.q.QueryContext(ctx,
		"SELECT occurred_at, actor_id, command, COALESCE(entity_id, ''), action, outcome, detail FROM command_audit_logs ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, fmt.Errorf("query audit log: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var at, outcome string
		if err := rows.Scan(&at, &e.ActorID, &e.Command, &e.EntityID, &e.Action, &outcome, &e.Detail); err != nil {
			return nil, fmt.Errorf("scan audit entry: %w", err)
		}
		if e.Time, err = storage.ParseTime(at); err != nil {
			return nil, fmt.Errorf("parse audit time: %w", err)
		}
		e.Outcome = Outcome(outcome)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query audit log: %w", err)
	}
	return out, nil
}

// Prune deletes entries older than cutoff and returns how many were removed.
func (l *Log) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := l.q.ExecContext(ctx, "DELETE FROM command_audit_logs WHERE occurred_at < ?", storage.FormatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("prune audit log: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune audit log: %w", err)
	}
	return n, nil
}

// RunRetention prunes entries older than retention at start-up and every few
// hours until ctx is cancelled.
func (l *Log) RunRetention(ctx context.Context, retention time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()
	for {
		n, err := l.Prune(ctx, time.Now().Add(-retention))
		if err == nil {
			var extra int64
			if extra, err = l.PruneDenied(ctx, MaxDeniedRows); err == nil {
				n += extra
			}
		}
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			logger.Error("audit retention failed", "error", err)
		case n > 0:
			logger.Info("pruned audit log", "removed", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
