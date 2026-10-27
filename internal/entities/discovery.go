package entities

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// SnapshotTTL is how long a discovery snapshot is reused before Home
	// Assistant is asked again (Refresh, /sync and apply always ask).
	SnapshotTTL = 30 * time.Second
	// PageSize is the number of entities per wizard page.
	PageSize = 10
	// maxOptionLen is Discord's limit for a select option value.
	maxOptionLen = 100
)

// Item is the compact view of an entity that discovery keeps in memory. Names,
// states and classes come straight from Home Assistant and are never stored.
type Item struct {
	ID          ID
	Name        string
	State       string
	DeviceClass string
	Unit        string
}

// Domain returns the entity's domain.
func (i Item) Domain() Domain { return i.ID.Domain() }

// Reliable reports whether the entity reported a real value.
func (i Item) Reliable() bool { return i.State != StateUnavailable && i.State != StateUnknown }

// Snapshot is an immutable view of Home Assistant's entities at one moment.
type Snapshot struct {
	// Items are the entities that can be offered in a select menu, sorted by
	// name then ID.
	Items []Item
	// Skipped counts entities whose ID is too long to offer in a select menu.
	Skipped   int
	FetchedAt time.Time
	all       map[ID]Item
}

func newSnapshot(states []State, at time.Time) *Snapshot {
	s := &Snapshot{FetchedAt: at, all: make(map[ID]Item, len(states))}
	for _, st := range states {
		item := Item{ID: st.ID, Name: st.FriendlyName(), State: st.State, DeviceClass: st.DeviceClass(), Unit: st.Unit()}
		s.all[st.ID] = item
		if len(st.ID) > maxOptionLen {
			s.Skipped++
			continue
		}
		s.Items = append(s.Items, item)
	}
	slices.SortFunc(s.Items, func(a, b Item) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	return s
}

// Find returns the entity with the given ID, including IDs too long for a menu.
func (s *Snapshot) Find(id ID) (Item, bool) {
	item, ok := s.all[id]
	return item, ok
}

// Total is the number of entities Home Assistant reported.
func (s *Snapshot) Total() int { return len(s.all) }

// Filter narrows a Search. Every query word must appear in the name, ID or
// device class (case-insensitive).
type Filter struct {
	Query  string
	Domain Domain
	// Only, when non-nil, restricts results to these IDs (for example the shared entities).
	Only map[ID]struct{}
}

// Search returns the matching items in the snapshot's order.
func (s *Snapshot) Search(f Filter) []Item {
	words := strings.Fields(strings.ToLower(f.Query))
	var out []Item
	for _, item := range s.Items {
		if f.Domain != "" && item.Domain() != f.Domain {
			continue
		}
		if f.Only != nil {
			if _, ok := f.Only[item.ID]; !ok {
				continue
			}
		}
		hay := strings.ToLower(item.Name + " " + string(item.ID) + " " + item.DeviceClass)
		match := true
		for _, w := range words {
			if !strings.Contains(hay, w) {
				match = false
				break
			}
		}
		if match {
			out = append(out, item)
		}
	}
	return out
}

// DomainCount is how many menu-eligible entities a domain has.
type DomainCount struct {
	Domain Domain
	Count  int
}

// Domains lists the domains present, most populous first.
func (s *Snapshot) Domains() []DomainCount {
	counts := map[Domain]int{}
	for _, item := range s.Items {
		counts[item.Domain()]++
	}
	out := make([]DomainCount, 0, len(counts))
	for d, n := range counts {
		out = append(out, DomainCount{Domain: d, Count: n})
	}
	slices.SortFunc(out, func(a, b DomainCount) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return strings.Compare(string(a.Domain), string(b.Domain))
	})
	return out
}

// Paginate returns one page of items. An out-of-range page is clamped; the
// result also carries the page number actually used and the page count.
func Paginate[T any](items []T, page, size int) (slice []T, current, pages int) {
	pages = max(1, (len(items)+size-1)/size)
	current = min(max(page, 1), pages)
	start := (current - 1) * size
	return items[start:min(start+size, len(items))], current, pages
}

// StateSource is the part of the Home Assistant client discovery needs.
type StateSource interface {
	States(ctx context.Context) ([]State, error)
}

// DiscoveryDeps are the collaborators of Discovery.
type DiscoveryDeps struct {
	HA StateSource
	DB interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	}
	Permissions *Permissions
	Now         func() time.Time
}

// Discovery caches one snapshot of Home Assistant's entities, shared by the
// wizard and /sync, so paging and searching do not hit Home Assistant.
type Discovery struct {
	d    DiscoveryDeps
	mu   sync.Mutex
	snap *Snapshot
}

// NewDiscovery returns a Discovery.
func NewDiscovery(d DiscoveryDeps) *Discovery { return &Discovery{d: d} }

// Snapshot returns the cached snapshot, or fetches a fresh one when force is
// set or the cache is older than SnapshotTTL. Concurrent callers share one fetch.
func (d *Discovery) Snapshot(ctx context.Context, force bool) (*Snapshot, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !force && d.snap != nil && d.d.Now().Sub(d.snap.FetchedAt) < SnapshotTTL {
		return d.snap, nil
	}
	states, err := d.d.HA.States(ctx)
	if err != nil {
		return nil, fmt.Errorf("discover entities: %w", err)
	}
	d.snap = newSnapshot(states, d.d.Now())
	return d.snap, nil
}
