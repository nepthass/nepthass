package entities

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nepthass/nepthass/internal/storage"
)

// SyncReport is the outcome of a synchronization with Home Assistant.
type SyncReport struct {
	At time.Time
	// FirstSync is true when no entity had been recorded before; the baseline is
	// stored and nothing is reported as new.
	FirstSync bool
	Total     int
	// Controllable counts entities in the control allowlist (lights and switches).
	Controllable int
	Skipped      int
	// New entities appeared since the previous sync, Removed disappeared, and
	// Returned came back after having been missing.
	New, Removed, Returned []ID
	// SharedMissing are shared entities Home Assistant no longer reports;
	// SharedUnavailable are shared entities that report unavailable or unknown.
	SharedMissing, SharedUnavailable []ID
}

// Sync fetches Home Assistant's entities, records the set of known entity IDs,
// and reports what changed. It never grants access and never changes a
// permission or an alias: new entities stay private until an administrator
// shares them. Only IDs and first/last seen times are stored.
func (d *Discovery) Sync(ctx context.Context) (SyncReport, error) {
	snap, err := d.Snapshot(ctx, true)
	if err != nil {
		return SyncReport{}, err
	}
	perms, err := d.d.Permissions.List(ctx)
	if err != nil {
		return SyncReport{}, err
	}
	report := SyncReport{At: d.d.Now(), Total: snap.Total(), Skipped: snap.Skipped}
	for _, item := range snap.all {
		if item.Domain().Controllable() {
			report.Controllable++
		}
	}
	for _, p := range perms {
		switch item, ok := snap.Find(p.ID); {
		case !ok:
			report.SharedMissing = append(report.SharedMissing, p.ID)
		case !item.Reliable():
			report.SharedUnavailable = append(report.SharedUnavailable, p.ID)
		}
	}
	if err := d.record(ctx, snap, &report); err != nil {
		return SyncReport{}, err
	}
	return report, nil
}

// record updates known_entities in one transaction and fills the change lists.
func (d *Discovery) record(ctx context.Context, snap *Snapshot, report *SyncReport) (err error) {
	tx, err := d.d.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sync: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	existing, err := knownEntities(ctx, tx)
	if err != nil {
		return err
	}
	report.FirstSync = len(existing) == 0
	at := storage.FormatTime(report.At)
	for id, item := range snap.all {
		missing, known := existing[id]
		switch {
		case !known:
			_, err = tx.ExecContext(ctx, "INSERT INTO known_entities (entity_id, domain, first_seen_at, last_seen_at) VALUES (?, ?, ?, ?)", string(id), string(item.Domain()), at, at)
			if !report.FirstSync {
				report.New = append(report.New, id)
			}
		case missing:
			_, err = tx.ExecContext(ctx, "UPDATE known_entities SET last_seen_at = ?, missing_since = NULL WHERE entity_id = ?", at, string(id))
			report.Returned = append(report.Returned, id)
		default:
			_, err = tx.ExecContext(ctx, "UPDATE known_entities SET last_seen_at = ? WHERE entity_id = ?", at, string(id))
		}
		if err != nil {
			return fmt.Errorf("record entity: %w", err)
		}
	}
	for id, missing := range existing {
		if _, present := snap.all[id]; present || missing {
			continue
		}
		if _, err = tx.ExecContext(ctx, "UPDATE known_entities SET missing_since = ? WHERE entity_id = ?", at, string(id)); err != nil {
			return fmt.Errorf("record missing entity: %w", err)
		}
		report.Removed = append(report.Removed, id)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit sync: %w", err)
	}
	for _, list := range [][]ID{report.New, report.Removed, report.Returned, report.SharedMissing, report.SharedUnavailable} {
		slices.SortFunc(list, func(a, b ID) int { return strings.Compare(string(a), string(b)) })
	}
	return nil
}

// knownEntities maps every recorded entity ID to whether it is currently missing.
func knownEntities(ctx context.Context, tx *sql.Tx) (map[ID]bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT entity_id, missing_since IS NOT NULL FROM known_entities")
	if err != nil {
		return nil, fmt.Errorf("read known entities: %w", err)
	}
	defer rows.Close()
	out := make(map[ID]bool)
	for rows.Next() {
		var id string
		var missing bool
		if err := rows.Scan(&id, &missing); err != nil {
			return nil, fmt.Errorf("scan known entity: %w", err)
		}
		out[ID(id)] = missing
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read known entities: %w", err)
	}
	return out, nil
}
