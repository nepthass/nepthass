package entities

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nepthass/nepthass/internal/storage"
)

// MaxAliasRunes is the longest alias accepted.
const MaxAliasRunes = 32

var (
	// ErrInvalidAlias is returned for an alias that fails validation.
	ErrInvalidAlias = errors.New("invalid alias")
	// ErrAliasTaken is returned when another entity already uses the alias.
	ErrAliasTaken = errors.New("alias already in use")
)

// AliasTakenError names the entity that already holds the alias.
type AliasTakenError struct {
	Alias  string
	Holder ID
}

func (e *AliasTakenError) Error() string {
	if e.Holder == "" {
		return fmt.Sprintf("alias %q is already in use", e.Alias)
	}
	return fmt.Sprintf("alias %q is already used by %s", e.Alias, e.Holder)
}

// Is makes errors.Is(err, ErrAliasTaken) work.
func (e *AliasTakenError) Is(target error) bool { return target == ErrAliasTaken }

// NormalizeAlias validates raw user input. It returns the display form (outer
// whitespace trimmed, inner whitespace collapsed) and the case-folded key that
// makes aliases unique. Only letters, digits, combining marks, spaces and
// "- ' . ( )" are accepted, so an alias can never carry markdown, mentions or
// invisible characters, and it must not look like an entity ID.
func NormalizeAlias(raw string) (display, key string, err error) {
	display = strings.Join(strings.FieldsFunc(raw, unicode.IsSpace), " ")
	switch n := utf8.RuneCountInString(display); {
	case n == 0:
		return "", "", fmt.Errorf("%w: it is empty", ErrInvalidAlias)
	case n > MaxAliasRunes:
		return "", "", fmt.Errorf("%w: at most %d characters", ErrInvalidAlias, MaxAliasRunes)
	}
	for _, r := range display {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsMark(r) && !strings.ContainsRune(" -'.()", r) {
			return "", "", fmt.Errorf("%w: use letters, numbers, spaces and - ' . ( ) only", ErrInvalidAlias)
		}
	}
	key = strings.ToLower(display)
	if _, parseErr := ParseID(key); parseErr == nil {
		return "", "", fmt.Errorf("%w: it must not look like an entity ID", ErrInvalidAlias)
	}
	return display, key, nil
}

// Aliases stores entity aliases in SQLite.
type Aliases struct{ q queryer }

// NewAliases returns an alias store backed by db.
func NewAliases(db *sql.DB) *Aliases { return &Aliases{q: db} }

// WithTx returns a store whose operations run inside tx.
func (a *Aliases) WithTx(tx *sql.Tx) *Aliases { return &Aliases{q: tx} }

// AliasSet is a request to set the alias of one entity.
type AliasSet struct {
	ID    ID
	Alias string
	By    string
	At    time.Time
}

// Set creates or replaces the alias of an entity. A case-insensitive duplicate
// held by another entity is rejected with *AliasTakenError; nothing is renamed.
func (a *Aliases) Set(ctx context.Context, s AliasSet) error {
	display, key, err := NormalizeAlias(s.Alias)
	if err != nil {
		return err
	}
	var holder string
	err = a.q.QueryRowContext(ctx, "SELECT entity_id FROM entity_aliases WHERE alias_key = ? AND entity_id <> ?", key, string(s.ID)).Scan(&holder)
	switch {
	case err == nil:
		return &AliasTakenError{Alias: display, Holder: ID(holder)}
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("check alias: %w", err)
	}
	_, err = a.q.ExecContext(ctx, `INSERT INTO entity_aliases (entity_id, alias, alias_key, updated_by, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(entity_id) DO UPDATE SET alias = excluded.alias, alias_key = excluded.alias_key, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		string(s.ID), display, key, s.By, storage.FormatTime(s.At))
	if err != nil {
		if strings.Contains(err.Error(), "entity_aliases.alias_key") {
			return &AliasTakenError{Alias: display}
		}
		return fmt.Errorf("write alias: %w", err)
	}
	return nil
}

// Remove deletes an entity's alias and reports whether one existed.
func (a *Aliases) Remove(ctx context.Context, id ID) (bool, error) {
	res, err := a.q.ExecContext(ctx, "DELETE FROM entity_aliases WHERE entity_id = ?", string(id))
	if err != nil {
		return false, fmt.Errorf("remove alias: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("remove alias: %w", err)
	}
	return n > 0, nil
}

// Get returns the alias of an entity ("" when none).
func (a *Aliases) Get(ctx context.Context, id ID) (string, error) {
	var alias string
	err := a.q.QueryRowContext(ctx, "SELECT alias FROM entity_aliases WHERE entity_id = ?", string(id)).Scan(&alias)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read alias: %w", err)
	}
	return alias, nil
}

// All returns every alias by entity ID.
func (a *Aliases) All(ctx context.Context) (map[ID]string, error) {
	rows, err := a.q.QueryContext(ctx, "SELECT entity_id, alias FROM entity_aliases")
	if err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	defer rows.Close()
	out := make(map[ID]string)
	for rows.Next() {
		var id, alias string
		if err := rows.Scan(&id, &alias); err != nil {
			return nil, fmt.Errorf("scan alias: %w", err)
		}
		out[ID(id)] = alias
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	return out, nil
}

// Lookup finds the entity that holds a case-folded alias key.
func (a *Aliases) Lookup(ctx context.Context, key string) (ID, bool, error) {
	var id string
	err := a.q.QueryRowContext(ctx, "SELECT entity_id FROM entity_aliases WHERE alias_key = ?", key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("look up alias: %w", err)
	}
	return ID(id), true, nil
}
