package monitor

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/entities"
)

// ErrNotMonitored means the entity is shared with the caller but is not a
// sensor NeptHass monitors.
var ErrNotMonitored = errors.New("that entity is not a monitored sensor")

// PermissionSource is the part of entities.Permissions the Reader needs. The
// Reader asks it on every read: nothing about access is cached.
type PermissionSource interface {
	Get(ctx context.Context, id entities.ID) (entities.Access, error)
	List(ctx context.Context) ([]entities.Permission, error)
}

// AliasSource supplies display aliases.
type AliasSource interface {
	All(ctx context.Context) (map[entities.ID]string, error)
}

// Reader is the only way monitored data reaches a Discord user. The WebSocket
// ingestion path is authorized by Home Assistant's token and is unrelated to
// people; this is the user-facing boundary. Presence in the Tracker grants
// nothing: every read re-checks the caller's level and the entity's current
// permission, so revoking access takes effect on the next read, including for
// data that is already cached.
type Reader struct {
	m       *Monitor
	perms   PermissionSource
	aliases AliasSource
}

// NewReader returns a Reader over a Monitor.
func NewReader(m *Monitor, perms PermissionSource, aliases AliasSource) *Reader {
	return &Reader{m: m, perms: perms, aliases: aliases}
}

// Status reports monitoring health (aggregate, no entity data).
func (r *Reader) Status() Status { return r.m.Status() }

// View is one sensor as shown to a user.
type View struct {
	Sensor
	// Alias is the administrator-assigned name ("" when none).
	Alias string
	// Stale is true when monitoring is not healthy: the value is the last
	// known one and may be outdated.
	Stale bool
}

// DisplayName is the alias, else Home Assistant's friendly name, else the ID.
func (v View) DisplayName() string {
	switch {
	case v.Alias != "":
		return v.Alias
	case v.Name != "":
		return v.Name
	default:
		return string(v.ID)
	}
}

// Page is one page of the sensors a user may see.
type Page struct {
	Views  []View
	Page   int
	Pages  int
	Total  int
	Status Status
}

// Sensor returns one sensor if the caller may read it. An entity that is not
// shared is reported as access denied whether or not it is tracked, so the
// answer never reveals what exists.
func (r *Reader) Sensor(ctx context.Context, actor auth.Actor, id entities.ID) (View, error) {
	if err := actor.Require(auth.LevelUser); err != nil {
		return View{}, err
	}
	access, err := r.perms.Get(ctx, id)
	if err != nil {
		return View{}, err
	}
	if !access.AtLeast(entities.AccessRead) {
		return View{}, entities.ErrAccessDenied
	}
	s, ok := r.m.tracker.Get(id)
	if !ok {
		return View{}, ErrNotMonitored
	}
	aliases, err := r.aliases.All(ctx)
	if err != nil {
		return View{}, err
	}
	return View{Sensor: s, Alias: aliases[id], Stale: !r.m.Status().Healthy}, nil
}

// Sensors lists, ten per page, the monitored sensors the caller may read.
func (r *Reader) Sensors(ctx context.Context, actor auth.Actor, page int) (Page, error) {
	if err := actor.Require(auth.LevelUser); err != nil {
		return Page{}, err
	}
	perms, err := r.perms.List(ctx)
	if err != nil {
		return Page{}, err
	}
	aliases, err := r.aliases.All(ctx)
	if err != nil {
		return Page{}, err
	}
	status := r.m.Status()
	var views []View
	for _, p := range perms {
		if !p.Access.AtLeast(entities.AccessRead) {
			continue
		}
		if s, ok := r.m.tracker.Get(p.ID); ok {
			views = append(views, View{Sensor: s, Alias: aliases[p.ID], Stale: !status.Healthy})
		}
	}
	slices.SortFunc(views, func(a, b View) int {
		if c := strings.Compare(strings.ToLower(a.DisplayName()), strings.ToLower(b.DisplayName())); c != 0 {
			return c
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	slice, current, pages := entities.Paginate(views, page, entities.PageSize)
	return Page{Views: slice, Page: current, Pages: pages, Total: len(views), Status: status}, nil
}
