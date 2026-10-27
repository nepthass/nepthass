package setup

import (
	"context"
	"slices"
	"strings"

	"github.com/nepthass/nepthass/internal/entities"
)

// Kind classifies what applying an entry would do.
type Kind string

const (
	KindNew       Kind = "new"
	KindModified  Kind = "modified"
	KindRevoked   Kind = "revoked"
	KindUnchanged Kind = "unchanged"
)

// AliasChange classifies the alias part of an entry.
type AliasChange string

const (
	AliasUnchanged AliasChange = ""
	AliasAdded     AliasChange = "added"
	AliasUpdated   AliasChange = "updated"
	AliasRemoved   AliasChange = "removed"
)

// PlanEntry is one entity in the review.
type PlanEntry struct {
	ID      entities.ID
	Name    string
	Missing bool
	Current Base
	Desired Base
	Kind    Kind
	Alias   AliasChange
	// Stale means a field of the entity's live configuration differs both from
	// what it was when selected and from what the draft wants: it changed outside the session.
	Stale    bool
	Problems []string
}

// Plan is the reviewed difference between the live configuration and the draft.
type Plan struct {
	Entries []PlanEntry
	// Counts by kind; AliasChanges counts entries whose alias changes.
	New, Modified, Revoked, Unchanged, AliasChanges int
	// UntouchedShared counts shared entities that are not part of the draft.
	UntouchedShared int
}

// Problems returns everything that blocks applying the plan.
func (p Plan) Problems() []Problem {
	var out []Problem
	for _, e := range p.Entries {
		for _, reason := range e.Problems {
			out = append(out, Problem{ID: e.ID, Reason: reason})
		}
	}
	return out
}

// StaleIDs lists the entries that changed outside the session.
func (p Plan) StaleIDs() []entities.ID {
	var out []entities.ID
	for _, e := range p.Entries {
		if e.Stale {
			out = append(out, e.ID)
		}
	}
	return out
}

// Changes counts the entries that would write something.
func (p Plan) Changes() int { return p.New + p.Modified + p.Revoked }

func classify(cur, want Base) Kind {
	switch {
	case cur == want:
		return KindUnchanged
	case cur.Access == entities.AccessNone && want.Access != entities.AccessNone:
		return KindNew
	case cur.Access != entities.AccessNone && want.Access == entities.AccessNone:
		return KindRevoked
	default:
		return KindModified
	}
}

func aliasChange(cur, want string) AliasChange {
	switch {
	case cur == want:
		return AliasUnchanged
	case cur == "":
		return AliasAdded
	case want == "":
		return AliasRemoved
	default:
		return AliasUpdated
	}
}

// buildPlan is pure: it compares the draft with the live configuration and the
// live Home Assistant snapshot. It is used both for the review screen and, with
// transactional reads, inside apply.
func buildPlan(sess Session, snap *entities.Snapshot, cfg map[entities.ID]Base) Plan {
	var plan Plan
	finalAlias := map[string][]entities.ID{}
	for id, b := range cfg {
		if _, inDraft := sess.Draft.Entries[id]; !inDraft && b.Alias != "" {
			_, key, _ := entities.NormalizeAlias(b.Alias)
			finalAlias[key] = append(finalAlias[key], id)
		}
	}
	for id, e := range sess.Draft.Entries {
		if e.Desired.Alias != "" {
			if _, key, err := entities.NormalizeAlias(e.Desired.Alias); err == nil {
				finalAlias[key] = append(finalAlias[key], id)
			}
		}
	}
	for _, id := range sess.Selected() {
		e := sess.Draft.Entries[id]
		cur := cfg[id]
		item, present := snap.Find(id)
		pe := PlanEntry{ID: id, Name: id.String(), Missing: !present, Current: cur, Desired: e.Desired, Kind: classify(cur, e.Desired), Alias: aliasChange(cur.Alias, e.Desired.Alias)}
		if present {
			pe.Name = item.Name
		}
		// A field is stale when it changed outside the session to something other
		// than what the draft wants; a field already at the desired value is not a conflict.
		pe.Stale = cur.Access != e.Base.Access && cur.Access != e.Desired.Access ||
			cur.Alias != e.Base.Alias && cur.Alias != e.Desired.Alias
		if !present && (e.Desired.Access != entities.AccessNone || pe.Alias != AliasUnchanged && e.Desired.Alias != "") {
			pe.Problems = append(pe.Problems, "no longer exists in Home Assistant")
		}
		if err := entities.CheckGrant(id, e.Desired.Access); err != nil {
			pe.Problems = append(pe.Problems, "control is limited to lights and switches")
		}
		if e.Desired.Alias != "" {
			if _, key, err := entities.NormalizeAlias(e.Desired.Alias); err != nil {
				pe.Problems = append(pe.Problems, err.Error())
			} else if holders := finalAlias[key]; len(holders) > 1 {
				other := holders[0]
				if other == id {
					other = holders[1]
				}
				pe.Problems = append(pe.Problems, "alias \""+e.Desired.Alias+"\" is also used by "+string(other))
			}
		}
		plan.Entries = append(plan.Entries, pe)
		switch pe.Kind {
		case KindNew:
			plan.New++
		case KindModified:
			plan.Modified++
		case KindRevoked:
			plan.Revoked++
		case KindUnchanged:
			plan.Unchanged++
		}
		if pe.Alias != AliasUnchanged {
			plan.AliasChanges++
		}
	}
	for id, b := range cfg {
		if _, inDraft := sess.Draft.Entries[id]; !inDraft && b.Access != entities.AccessNone {
			plan.UntouchedShared++
		}
	}
	slices.SortFunc(plan.Entries, func(a, b PlanEntry) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	return plan
}

// Plan computes the review for a session from live data. Home Assistant must be
// reachable: a review is never shown from stale or invented data.
func (s *Service) Plan(ctx context.Context, c Caller, id string) (Plan, error) {
	sess, err := s.load(ctx, c, id)
	if err != nil {
		return Plan{}, err
	}
	if sess.Status != StatusActive {
		return Plan{}, ErrNotActive
	}
	return s.planFor(ctx, sess)
}

func (s *Service) planFor(ctx context.Context, sess Session) (Plan, error) {
	snap, err := s.d.Discovery.Snapshot(ctx, false)
	if err != nil {
		return Plan{}, &HomeAssistantError{Err: err}
	}
	cfg, err := s.currentConfig(ctx, s.d.Permissions, s.d.Aliases)
	if err != nil {
		return Plan{}, err
	}
	return buildPlan(sess, snap, cfg), nil
}
