package notify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/storage"
)

// queryer is satisfied by both *sql.DB and *sql.Tx.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store reads and writes the notification tables. It makes no authorization
// decision and writes no audit entry; Service does both.
type Store struct{ q queryer }

// NewStore returns a Store on db.
func NewStore(db *sql.DB) *Store { return &Store{q: db} }

// WithTx returns a Store that runs inside tx.
func (s *Store) WithTx(tx *sql.Tx) *Store { return &Store{q: tx} }

const ruleColumns = "id, entity_id, kind, threshold, clear_threshold, unit, duration_seconds, enabled, cooldown_seconds, created_by, created_at, updated_by, updated_at"

// GetSettings returns the settings of guildID; false when none were ever set.
func (s *Store) GetSettings(ctx context.Context, guildID string) (Settings, bool, error) {
	var st Settings
	var enabled int
	var updatedAt string
	var summaryAt sql.NullString
	err := s.q.QueryRowContext(ctx,
		"SELECT guild_id, channel_id, enabled, updated_by, updated_at, startup_summary_at FROM notification_settings WHERE guild_id = ?",
		guildID).Scan(&st.GuildID, &st.ChannelID, &enabled, &st.UpdatedBy, &updatedAt, &summaryAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Settings{}, false, nil
	}
	if err != nil {
		return Settings{}, false, fmt.Errorf("read notification settings: %w", err)
	}
	st.Enabled = enabled == 1
	if st.UpdatedAt, err = storage.ParseTime(updatedAt); err != nil {
		return Settings{}, false, fmt.Errorf("read notification settings: %w", err)
	}
	if summaryAt.Valid {
		if st.StartupSummaryAt, err = storage.ParseTime(summaryAt.String); err != nil {
			return Settings{}, false, fmt.Errorf("read notification settings: %w", err)
		}
	}
	return st, true, nil
}

// UpsertChannel sets the channel. A first call creates the settings enabled; a
// later call keeps the enabled flag.
func (s *Store) UpsertChannel(ctx context.Context, guildID, channelID, actor string, now time.Time) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO notification_settings (guild_id, channel_id, enabled, updated_by, updated_at)
VALUES (?, ?, 1, ?, ?)
ON CONFLICT (guild_id) DO UPDATE SET channel_id = excluded.channel_id, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		guildID, channelID, actor, storage.FormatTime(now))
	if err != nil {
		return fmt.Errorf("save notification channel: %w", err)
	}
	return nil
}

// SetEnabled changes the enabled flag; false when there are no settings yet.
func (s *Store) SetEnabled(ctx context.Context, guildID string, enabled bool, actor string, now time.Time) (bool, error) {
	res, err := s.q.ExecContext(ctx,
		"UPDATE notification_settings SET enabled = ?, updated_by = ?, updated_at = ? WHERE guild_id = ?",
		boolInt(enabled), actor, storage.FormatTime(now), guildID)
	if err != nil {
		return false, fmt.Errorf("save notification enabled flag: %w", err)
	}
	return affected(res)
}

// CountRules returns how many rules exist.
func (s *Store) CountRules(ctx context.Context) (int, error) {
	var n int
	if err := s.q.QueryRowContext(ctx, "SELECT count(*) FROM notification_rules").Scan(&n); err != nil {
		return 0, fmt.Errorf("count notification rules: %w", err)
	}
	return n, nil
}

// InsertRule stores r (ignoring r.ID) and returns its ID. It reports
// ErrRuleExists and ErrRuleLimit for the two constraints that have an answer.
func (s *Store) InsertRule(ctx context.Context, r Rule) (int64, error) {
	var threshold any
	if r.Threshold != nil {
		threshold = *r.Threshold
	}
	var clear any
	if r.Clear != nil {
		clear = *r.Clear
	}
	var cooldown any
	if r.Cooldown != nil {
		cooldown = int64(*r.Cooldown / time.Second)
	}
	res, err := s.q.ExecContext(ctx, `INSERT INTO notification_rules
(entity_id, kind, threshold, clear_threshold, unit, duration_seconds, enabled, cooldown_seconds, created_by, created_at, updated_by, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(r.Entity), string(r.Kind), threshold, clear, r.Unit, int64(r.Duration/time.Second), boolInt(r.Enabled), cooldown,
		r.CreatedBy, storage.FormatTime(r.CreatedAt), r.UpdatedBy, storage.FormatTime(r.UpdatedAt))
	if err != nil {
		switch msg := err.Error(); {
		case strings.Contains(msg, "UNIQUE constraint failed"):
			return 0, ErrRuleExists
		case strings.Contains(msg, "notification rule limit reached"):
			return 0, ErrRuleLimit
		}
		return 0, fmt.Errorf("save notification rule: %w", err)
	}
	return res.LastInsertId()
}

// GetRule returns one rule; false when it does not exist.
func (s *Store) GetRule(ctx context.Context, id int64) (Rule, bool, error) {
	r, err := scanRule(s.q.QueryRowContext(ctx, "SELECT "+ruleColumns+" FROM notification_rules WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Rule{}, false, nil
	}
	if err != nil {
		return Rule{}, false, fmt.Errorf("read notification rule: %w", err)
	}
	return r, true, nil
}

// ListRules returns every rule, oldest first.
func (s *Store) ListRules(ctx context.Context) ([]Rule, error) {
	rows, err := s.q.QueryContext(ctx, "SELECT "+ruleColumns+" FROM notification_rules ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("list notification rules: %w", err)
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, fmt.Errorf("list notification rules: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list notification rules: %w", err)
	}
	return out, nil
}

// SetRuleEnabled pauses or resumes a rule; false when it does not exist.
func (s *Store) SetRuleEnabled(ctx context.Context, id int64, enabled bool, actor string, now time.Time) (bool, error) {
	res, err := s.q.ExecContext(ctx,
		"UPDATE notification_rules SET enabled = ?, updated_by = ?, updated_at = ? WHERE id = ?",
		boolInt(enabled), actor, storage.FormatTime(now), id)
	if err != nil {
		return false, fmt.Errorf("save notification rule: %w", err)
	}
	return affected(res)
}

// SetRuleUnit binds a numeric rule to a unit. It changes only a rule whose unit is
// still empty, in one statement, so a rule is bound at most once and never rebound;
// false when the rule does not exist or is already bound.
func (s *Store) SetRuleUnit(ctx context.Context, id int64, unit, actor string, now time.Time) (bool, error) {
	res, err := s.q.ExecContext(ctx,
		"UPDATE notification_rules SET unit = ?, updated_by = ?, updated_at = ? WHERE id = ? AND unit = '' AND kind <> 'state'",
		unit, actor, storage.FormatTime(now), id)
	if err != nil {
		return false, fmt.Errorf("bind notification rule unit: %w", err)
	}
	return affected(res)
}

// GetMark returns the delivery mark of a hazard rule; false when there is none.
func (s *Store) GetMark(ctx context.Context, ruleID int64) (Mark, bool, error) {
	var m Mark
	var at string
	err := s.q.QueryRowContext(ctx, "SELECT rule_id, episode_key, delivered_at FROM notification_marks WHERE rule_id = ?", ruleID).Scan(&m.RuleID, &m.EpisodeKey, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Mark{}, false, nil
	}
	if err != nil {
		return Mark{}, false, fmt.Errorf("read notification mark: %w", err)
	}
	if m.DeliveredAt, err = storage.ParseTime(at); err != nil {
		return Mark{}, false, fmt.Errorf("read notification mark: %w", err)
	}
	return m, true, nil
}

// PutMark records (or replaces) the delivery mark of a hazard rule. A rule that
// no longer exists is a foreign-key error, which the caller logs and ignores.
func (s *Store) PutMark(ctx context.Context, m Mark) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO notification_marks (rule_id, episode_key, delivered_at) VALUES (?, ?, ?)
ON CONFLICT (rule_id) DO UPDATE SET episode_key = excluded.episode_key, delivered_at = excluded.delivered_at`,
		m.RuleID, m.EpisodeKey, storage.FormatTime(m.DeliveredAt))
	if err != nil {
		return fmt.Errorf("save notification mark: %w", err)
	}
	return nil
}

// SetStartupSummary records when a start-up summary was delivered; false when
// there are no settings yet.
func (s *Store) SetStartupSummary(ctx context.Context, guildID string, at time.Time) (bool, error) {
	res, err := s.q.ExecContext(ctx, "UPDATE notification_settings SET startup_summary_at = ? WHERE guild_id = ?", storage.FormatTime(at), guildID)
	if err != nil {
		return false, fmt.Errorf("save start-up summary time: %w", err)
	}
	return affected(res)
}

// DeleteRule removes a rule; false when it does not exist.
func (s *Store) DeleteRule(ctx context.Context, id int64) (bool, error) {
	res, err := s.q.ExecContext(ctx, "DELETE FROM notification_rules WHERE id = ?", id)
	if err != nil {
		return false, fmt.Errorf("delete notification rule: %w", err)
	}
	return affected(res)
}

type scanner interface{ Scan(dest ...any) error }

func scanRule(sc scanner) (Rule, error) {
	var r Rule
	var id, kind string
	var threshold, clear sql.NullFloat64
	var cooldown sql.NullInt64
	var durationSeconds int64
	var enabled int
	var createdAt, updatedAt string
	if err := sc.Scan(&r.ID, &id, &kind, &threshold, &clear, &r.Unit, &durationSeconds, &enabled, &cooldown, &r.CreatedBy, &createdAt, &r.UpdatedBy, &updatedAt); err != nil {
		return Rule{}, err
	}
	r.Entity, r.Kind, r.Enabled = entities.ID(id), Kind(kind), enabled == 1
	r.Duration = time.Duration(durationSeconds) * time.Second
	if threshold.Valid {
		v := threshold.Float64
		r.Threshold = &v
	}
	if clear.Valid {
		v := clear.Float64
		r.Clear = &v
	}
	if cooldown.Valid {
		d := time.Duration(cooldown.Int64) * time.Second
		r.Cooldown = &d
	}
	var err error
	if r.CreatedAt, err = storage.ParseTime(createdAt); err != nil {
		return Rule{}, err
	}
	if r.UpdatedAt, err = storage.ParseTime(updatedAt); err != nil {
		return Rule{}, err
	}
	return r, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func affected(res sql.Result) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check rows affected: %w", err)
	}
	return n > 0, nil
}
