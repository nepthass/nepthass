// Package entities holds the Home Assistant entity model and the rules that
// decide who may see or control an entity.
package entities

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidID is returned when a string is not a well-formed entity ID.
var ErrInvalidID = errors.New("invalid entity ID")

const (
	minIDLen = 3 // "a.b"
	maxIDLen = 255
)

// Domain is the part of an entity ID before the dot ("light", "switch", "lock", ...).
type Domain string

const (
	DomainLight  Domain = "light"
	DomainSwitch Domain = "switch"
)

// Controllable reports whether NeptHass may send commands to this domain.
// It is a deny-by-default allowlist: locks, covers (garage doors), alarm
// panels, climate and every other domain are read-only or unsupported until a
// security review adds them here.
func (d Domain) Controllable() bool {
	switch d {
	case DomainLight, DomainSwitch:
		return true
	default:
		return false
	}
}

// ID is a validated entity ID such as "light.kitchen". Obtain one with
// ParseID; the zero value is not a valid ID.
type ID string

// ParseID validates s. Only lowercase letters, digits and underscores are
// accepted on each side of the single dot, which also makes the ID safe to
// embed in URL paths and SQL parameters without escaping.
func ParseID(s string) (ID, error) {
	if len(s) < minIDLen || len(s) > maxIDLen {
		return "", fmt.Errorf("%w: length must be %d-%d", ErrInvalidID, minIDLen, maxIDLen)
	}
	domain, object, found := strings.Cut(s, ".")
	if !found || domain == "" || object == "" || !idPart(domain) || !idPart(object) {
		return "", fmt.Errorf("%w: expected domain.object_id using a-z, 0-9 and _", ErrInvalidID)
	}
	return ID(s), nil
}

func idPart(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// Domain returns the part before the dot.
func (id ID) Domain() Domain {
	domain, _, _ := strings.Cut(string(id), ".")
	return Domain(domain)
}

// String implements fmt.Stringer.
func (id ID) String() string { return string(id) }

// Action is a power command for a controllable entity.
type Action string

const (
	ActionOn  Action = "on"
	ActionOff Action = "off"
)

// ErrInvalidAction is returned for an unknown action name.
var ErrInvalidAction = errors.New("invalid action")

// ParseAction validates a user-supplied action name.
func ParseAction(s string) (Action, error) {
	switch a := Action(s); a {
	case ActionOn, ActionOff:
		return a, nil
	default:
		return "", fmt.Errorf("%w: expected on or off", ErrInvalidAction)
	}
}

// Service returns the Home Assistant service name for the action.
func (a Action) Service() string {
	if a == ActionOn {
		return "turn_on"
	}
	return "turn_off"
}

// Home Assistant's reserved states for entities it cannot read.
const (
	StateUnavailable = "unavailable"
	StateUnknown     = "unknown"
)

// State is a snapshot of one entity as reported by Home Assistant.
type State struct {
	ID          ID
	State       string
	Attributes  map[string]any
	LastChanged time.Time
	LastUpdated time.Time
	// Alias is the administrator-assigned alias, filled in by Service for
	// entities the caller may see ("" when none).
	Alias string
}

// Reliable reports whether the entity reported a real value. Unavailable and
// unknown must never be interpreted as "off", "closed" or "safe".
func (s State) Reliable() bool {
	return s.State != StateUnavailable && s.State != StateUnknown
}

// FriendlyName returns the friendly_name attribute, or the entity ID when absent.
func (s State) FriendlyName() string {
	if name := s.attrString("friendly_name"); name != "" {
		return name
	}
	return string(s.ID)
}

// DisplayName is the alias when one is set, otherwise Home Assistant's friendly name.
func (s State) DisplayName() string {
	if s.Alias != "" {
		return s.Alias
	}
	return s.FriendlyName()
}

// DeviceClass returns the device_class attribute ("" when absent).
func (s State) DeviceClass() string { return s.attrString("device_class") }

// Unit returns the unit_of_measurement attribute ("" when absent).
func (s State) Unit() string { return s.attrString("unit_of_measurement") }

func (s State) attrString(key string) string {
	v, _ := s.Attributes[key].(string)
	return v
}
