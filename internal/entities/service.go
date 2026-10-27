package entities

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/auth"
)

const (
	// DevicePageSize is the number of devices shown per page.
	DevicePageSize = 10
	// MaxSuggestions is the Discord autocomplete limit.
	MaxSuggestions = 25
	auditTimeout   = 5 * time.Second
)

var (
	// ErrAccessDenied means the entity has no sufficient permission row. It is
	// returned for unknown and forbidden entities alike so existence is not revealed.
	ErrAccessDenied = errors.New("access to this entity is not permitted")
	// ErrNotControllable means the entity's domain is outside the control allowlist.
	ErrNotControllable = errors.New("this entity's domain cannot be controlled")
)

// HomeAssistant is the subset of the REST client the service needs.
type HomeAssistant interface {
	State(ctx context.Context, id ID) (State, error)
	States(ctx context.Context) ([]State, error)
	Control(ctx context.Context, id ID, action Action) error
}

// Recorder persists audit entries.
type Recorder interface {
	Record(ctx context.Context, e audit.Entry) error
}

// Deps are the collaborators of Service.
type Deps struct {
	HA          HomeAssistant
	Permissions *Permissions
	Audit       Recorder
	Logger      *slog.Logger
	Now         func() time.Time
}

// Service implements the NeptHass entity rules: deny by default, explicit
// per-entity allowlist, control limited to allowlisted domains, every control
// attempt audited. It knows nothing about Discord.
type Service struct{ d Deps }

// NewService builds a Service.
func NewService(d Deps) *Service { return &Service{d: d} }

// Status returns the state of an entity the actor may read.
func (s *Service) Status(ctx context.Context, actor auth.Actor, id ID) (State, error) {
	if err := actor.Require(auth.LevelUser); err != nil {
		return State{}, err
	}
	access, err := s.d.Permissions.Get(ctx, id)
	if err != nil {
		return State{}, err
	}
	if !access.AtLeast(AccessRead) {
		return State{}, ErrAccessDenied
	}
	return s.d.HA.State(ctx, id)
}

// Device is a permitted entity with its live state (nil when Home Assistant no longer knows it).
type Device struct {
	ID     ID
	Access Access
	State  *State
}

// DevicePage is one page of permitted devices.
type DevicePage struct {
	Devices []Device
	Page    int
	Pages   int
	Total   int
}

// Devices lists permitted entities, a page at a time. Out-of-range pages are clamped.
func (s *Service) Devices(ctx context.Context, actor auth.Actor, page int) (DevicePage, error) {
	if err := actor.Require(auth.LevelUser); err != nil {
		return DevicePage{}, err
	}
	perms, err := s.d.Permissions.List(ctx)
	if err != nil {
		return DevicePage{}, err
	}
	total := len(perms)
	pages := max(1, (total+DevicePageSize-1)/DevicePageSize)
	page = min(max(page, 1), pages)
	result := DevicePage{Page: page, Pages: pages, Total: total}
	if total == 0 {
		return result, nil
	}
	start := (page - 1) * DevicePageSize
	slice := perms[start:min(start+DevicePageSize, total)]

	states, err := s.d.HA.States(ctx)
	if err != nil {
		return DevicePage{}, err
	}
	byID := make(map[ID]State, len(states))
	for _, st := range states {
		byID[st.ID] = st
	}
	for _, p := range slice {
		d := Device{ID: p.ID, Access: p.Access}
		if st, ok := byID[p.ID]; ok {
			d.State = &st
		}
		result.Devices = append(result.Devices, d)
	}
	return result, nil
}

// Suggest returns permitted entity IDs holding at least the given access whose
// ID contains query (case-insensitive), for Discord autocomplete.
func (s *Service) Suggest(ctx context.Context, actor auth.Actor, minimum Access, query string) ([]ID, error) {
	if err := actor.Require(auth.LevelUser); err != nil {
		return nil, err
	}
	perms, err := s.d.Permissions.List(ctx)
	if err != nil {
		return nil, err
	}
	query = strings.ToLower(strings.TrimSpace(query))
	var ids []ID
	for _, p := range perms {
		if p.Access.AtLeast(minimum) && strings.Contains(string(p.ID), query) {
			ids = append(ids, p.ID)
			if len(ids) == MaxSuggestions {
				break
			}
		}
	}
	return ids, nil
}
