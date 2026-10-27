package setup

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/storage"
)

var (
	// ErrNotFound is returned for a session that does not exist. It is also
	// returned for a session that belongs to someone else, so existence is not revealed.
	ErrNotFound = errors.New("setup session not found")
	// ErrConflict means the session changed since it was read (optimistic concurrency).
	ErrConflict = errors.New("setup session was changed by another action")
	// ErrActiveExists means the administrator already has an active session in the guild.
	ErrActiveExists = errors.New("an active setup session already exists")
)

type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store persists sessions in SQLite.
type Store struct{ q queryer }

// NewStore returns a store backed by db.
func NewStore(db *sql.DB) *Store { return &Store{q: db} }

// WithTx returns a store whose operations run inside tx.
func (s *Store) WithTx(tx *sql.Tx) *Store { return &Store{q: tx} }

// newID returns an unguessable session ID.
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

const sessionColumns = "id, guild_id, admin_id, step, status, version, state, created_at, updated_at, expires_at"

// Create inserts a new session.
func (s *Store) Create(ctx context.Context, sess Session) error {
	state, err := json.Marshal(sess.Draft)
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	_, err = s.q.ExecContext(ctx, "INSERT INTO setup_sessions ("+sessionColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		sess.ID, sess.GuildID, sess.AdminID, string(sess.Step), string(sess.Status), sess.Version, string(state),
		storage.FormatTime(sess.CreatedAt), storage.FormatTime(sess.UpdatedAt), storage.FormatTime(sess.ExpiresAt))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrActiveExists
		}
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// Get loads a session by ID.
func (s *Store) Get(ctx context.Context, id string) (Session, error) {
	return s.scan(s.q.QueryRowContext(ctx, "SELECT "+sessionColumns+" FROM setup_sessions WHERE id = ?", id))
}

// Active loads the active session of an administrator in a guild.
func (s *Store) Active(ctx context.Context, guildID, adminID string) (Session, error) {
	return s.scan(s.q.QueryRowContext(ctx, "SELECT "+sessionColumns+" FROM setup_sessions WHERE guild_id = ? AND admin_id = ? AND status = 'active'", guildID, adminID))
}

func (s *Store) scan(row *sql.Row) (Session, error) {
	var sess Session
	var step, status, state, created, updated, expires string
	err := row.Scan(&sess.ID, &sess.GuildID, &sess.AdminID, &step, &status, &sess.Version, &state, &created, &updated, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("read session: %w", err)
	}
	sess.Step, sess.Status = Step(step), Status(status)
	if err := json.Unmarshal([]byte(state), &sess.Draft); err != nil {
		return Session{}, fmt.Errorf("decode session: %w", err)
	}
	if sess.Draft.Entries == nil {
		sess.Draft.Entries = map[entities.ID]Entry{}
	}
	for _, p := range []struct {
		dst *time.Time
		src string
	}{{&sess.CreatedAt, created}, {&sess.UpdatedAt, updated}, {&sess.ExpiresAt, expires}} {
		if *p.dst, err = storage.ParseTime(p.src); err != nil {
			return Session{}, fmt.Errorf("decode session time: %w", err)
		}
	}
	return sess, nil
}

// Save writes the session if it is still active at sess.Version, then
// increments sess.Version. It is a compare-and-swap: when another action got
// there first, nothing is written and ErrConflict is returned.
func (s *Store) Save(ctx context.Context, sess *Session) error {
	state, err := json.Marshal(sess.Draft)
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	res, err := s.q.ExecContext(ctx, `UPDATE setup_sessions
		SET step = ?, status = ?, version = version + 1, state = ?, updated_at = ?, expires_at = ?
		WHERE id = ? AND version = ? AND status = 'active'`,
		string(sess.Step), string(sess.Status), string(state), storage.FormatTime(sess.UpdatedAt), storage.FormatTime(sess.ExpiresAt), sess.ID, sess.Version)
	if err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	if n == 0 {
		return ErrConflict
	}
	sess.Version++
	return nil
}

// Prune deletes sessions that are no longer active and were last touched before cutoff.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.q.ExecContext(ctx, "DELETE FROM setup_sessions WHERE status <> 'active' AND updated_at < ?", storage.FormatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("prune sessions: %w", err)
	}
	return res.RowsAffected()
}
