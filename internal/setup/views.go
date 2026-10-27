package setup

import (
	"context"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
)

// DiscoverItem is one offered entity with its sharing status.
type DiscoverItem struct {
	entities.Item
	Access   entities.Access
	Alias    string
	Selected bool
	// Gone is true when the entity vanished from Home Assistant since the page was offered.
	Gone bool
}

// DiscoverView is everything the discovery panel shows.
type DiscoverView struct {
	Items     []DiscoverItem
	Page      int
	Pages     int
	Matches   int
	Total     int
	Skipped   int
	Selected  int
	FetchedAt time.Time
	Domains   []entities.DomainCount
}

// Discover builds the discovery panel for a session from the cached snapshot.
func (s *Service) Discover(ctx context.Context, sess Session) (DiscoverView, error) {
	snap, err := s.d.Discovery.Snapshot(ctx, false)
	if err != nil {
		return DiscoverView{}, &HomeAssistantError{Err: err}
	}
	cfg, err := s.currentConfig(ctx, s.d.Permissions, s.d.Aliases)
	if err != nil {
		return DiscoverView{}, err
	}
	f, err := s.filter(ctx, sess.Draft.View)
	if err != nil {
		return DiscoverView{}, err
	}
	matches := snap.Search(f)
	_, current, pages := entities.Paginate(matches, sess.Draft.View.Page, entities.PageSize)
	view := DiscoverView{Page: current, Pages: pages, Matches: len(matches), Total: snap.Total(), Skipped: snap.Skipped, Selected: len(sess.Draft.Entries), FetchedAt: snap.FetchedAt, Domains: snap.Domains()}
	for _, id := range sess.Draft.View.Offered {
		item, ok := snap.Find(id)
		if !ok {
			item = entities.Item{ID: id, Name: id.String(), State: "missing"}
		}
		_, selected := sess.Draft.Entries[id]
		view.Items = append(view.Items, DiscoverItem{Item: item, Access: cfg[id].Access, Alias: cfg[id].Alias, Selected: selected, Gone: !ok})
	}
	return view, nil
}

// DraftItem is one selected entity in the permissions and alias panels.
type DraftItem struct {
	ID      entities.ID
	Name    string
	Entry   Entry
	Current Base
	Missing bool
}

// DraftView is a page of the selected entities.
type DraftView struct {
	Items []DraftItem
	Page  int
	Pages int
	Total int
	Focus *DraftItem
}

// Draft lists the session's selected entities, a page at a time. Names come
// from the cached snapshot when Home Assistant is reachable; display never
// fails because of it (applying and reviewing do).
func (s *Service) Draft(ctx context.Context, sess Session) (DraftView, error) {
	snap, _ := s.d.Discovery.Snapshot(ctx, false)
	cfg, err := s.currentConfig(ctx, s.d.Permissions, s.d.Aliases)
	if err != nil {
		return DraftView{}, err
	}
	item := func(id entities.ID) DraftItem {
		d := DraftItem{ID: id, Name: id.String(), Entry: sess.Draft.Entries[id], Current: cfg[id]}
		if snap != nil {
			if it, ok := snap.Find(id); ok {
				d.Name = it.Name
			} else {
				d.Missing = true
			}
		}
		return d
	}
	ids := sess.Selected()
	page, current, pages := entities.Paginate(ids, sess.Draft.View.Page, entities.PageSize)
	view := DraftView{Page: current, Pages: pages, Total: len(ids)}
	for _, id := range page {
		view.Items = append(view.Items, item(id))
	}
	if focus := entities.ID(sess.Draft.View.Focus); focus != "" {
		if _, ok := sess.Draft.Entries[focus]; ok {
			f := item(focus)
			view.Focus = &f
		}
	}
	return view, nil
}
