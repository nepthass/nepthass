package entities

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/nepthass/nepthass/internal/storage"
)

// Access is the level of access granted on one entity.
type Access string

const (
	// AccessNone is the default: the entity is invisible to NeptHass users.
	AccessNone Access = ""
	// AccessRead allows status queries.
	AccessRead Access = "read"
	// AccessControl allows status queries and on/off commands (light and switch only).
	AccessControl Access = "control"
)

// ErrInvalidAccess is returned for an unknown access level name.
var ErrInvalidAccess = errors.New("invalid access level")

// ParseAccess validates a user-supplied access level; AccessNone is not grantable.
func ParseAccess(s string) (Access, error) {
	switch a := Access(s); a {
	case AccessRead, AccessControl:
		return a, nil
	default:
		return AccessNone, fmt.Errorf("%w: expected read or control", ErrInvalidAccess)
	}
}

func (a Access) rank() int {
	switch a {
	case AccessControl:
		return 2
	case AccessRead:
		return 1
	default:
		return 0
	}
}

// AtLeast reports whether a grants at least the access of minimum.
func (a Access) AtLeast(minimum Access) bool { return a.rank() >= minimum.rank() }

// Permission is one stored allowlist entry.
type Permission struct {
	ID        ID
	Access    Access
	GrantedBy string
	UpdatedAt time.Time
}

// Grant is a request to set the access level of one entity.
type Grant struct {
	ID        ID
	Access    Access
	GrantedBy string
	At        time.Time
}

// queryer is satisfied by both *sql.DB and *sql.Tx, so the same store code runs
// standalone or inside a larger transaction.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Permissions stores the explicit entity allowlist in SQLite.
type Permissions struct{ q queryer }

// NewPermissions returns a permission store backed by db.
func NewPermissions(db *sql.DB) *Permissions { return &Permissions{q: db} }

// WithTx returns a store whose operations run inside tx.
func (p *Permissions) WithTx(tx *sql.Tx) *Permissions { return &Permissions{q: tx} }

// Get returns the access level of an entity; AccessNone when no row exists.
func (p *Permissions) Get(ctx context.Context, id ID) (Access, error) {
	var access string
	err := p.q.QueryRowContext(ctx, "SELECT access FROM entity_permissions WHERE entity_id = ?", string(id)).Scan(&access)
	if errors.Is(err, sql.ErrNoRows) {
		return AccessNone, nil
	}
	if err != nil {
		return AccessNone, fmt.Errorf("read permission: %w", err)
	}
	return Access(access), nil
}

// Set inserts or updates the access level of an entity.
func (p *Permissions) Set(ctx context.Context, g Grant) error {
	_, err := p.q.ExecContext(ctx, `INSERT INTO entity_permissions (entity_id, access, granted_by, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(entity_id) DO UPDATE SET access = excluded.access, granted_by = excluded.granted_by, updated_at = excluded.updated_at`,
		string(g.ID), string(g.Access), g.GrantedBy, storage.FormatTime(g.At))
	if err != nil {
		return fmt.Errorf("write permission: %w", err)
	}
	return nil
}

// Revoke removes an entity from the allowlist and reports whether a row existed.
func (p *Permissions) Revoke(ctx context.Context, id ID) (bool, error) {
	res, err := p.q.ExecContext(ctx, "DELETE FROM entity_permissions WHERE entity_id = ?", string(id))
	if err != nil {
		return false, fmt.Errorf("revoke permission: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("revoke permission: %w", err)
	}
	return n > 0, nil
}

// List returns every entry ordered by entity ID.
func (p *Permissions) List(ctx context.Context) ([]Permission, error) {
	rows, err := p.q.QueryContext(ctx, "SELECT entity_id, access, granted_by, updated_at FROM entity_permissions ORDER BY entity_id")
	if err != nil {
		return nil, fmt.Errorf("list permissions: %w", err)
	}
	defer rows.Close()
	var out []Permission
	for rows.Next() {
		var rawID, access, by, at string
		if err := rows.Scan(&rawID, &access, &by, &at); err != nil {
			return nil, fmt.Errorf("scan permission: %w", err)
		}
		id, err := ParseID(rawID)
		if err != nil {
			return nil, fmt.Errorf("stored permission: %w", err)
		}
		updated, err := storage.ParseTime(at)
		if err != nil {
			return nil, fmt.Errorf("stored permission time: %w", err)
		}
		out = append(out, Permission{ID: id, Access: Access(access), GrantedBy: by, UpdatedAt: updated})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list permissions: %w", err)
	}
	return out, nil
}
