package monitor

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nepthass/nepthass/internal/entities"
)

// Limits on untrusted Home Assistant data.
const (
	maxStateLen  = 255 // Home Assistant's own state length limit
	maxClassLen  = 64
	maxUnitLen   = 32
	maxNameRunes = 128
)

var (
	// ErrInvalid means a payload violated the expected shape or limits.
	ErrInvalid = errors.New("invalid home assistant payload")
	// errIgnored means the payload is valid but not about a monitored domain
	// or not a state_changed event. It is not an error worth counting.
	errIgnored = errors.New("payload ignored")
)

// Update is one validated observation of one entity.
type Update struct {
	ID          entities.ID
	Category    Category // empty: a sensor domain entity we do not classify
	Name        string
	DeviceClass string
	Unit        string
	Raw         string
	LastChanged time.Time
	LastUpdated time.Time
	// Removed marks an entity that no longer exists (new_state null).
	// PrevUpdated is the removed state's last_updated, used to refuse a
	// removal that is older than what the tracker already knows.
	Removed     bool
	PrevUpdated time.Time
}

type haAttributes struct {
	DeviceClass  json.RawMessage `json:"device_class"`
	Unit         json.RawMessage `json:"unit_of_measurement"`
	FriendlyName json.RawMessage `json:"friendly_name"`
}

type haState struct {
	EntityID    string          `json:"entity_id"`
	State       json.RawMessage `json:"state"`
	Attributes  haAttributes    `json:"attributes"`
	LastChanged string          `json:"last_changed"`
	LastUpdated string          `json:"last_updated"`
}

type haEvent struct {
	EventType string `json:"event_type"`
	Data      struct {
		EntityID string          `json:"entity_id"`
		OldState json.RawMessage `json:"old_state"`
		NewState json.RawMessage `json:"new_state"`
	} `json:"data"`
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// ParseState validates one Home Assistant state object (from get_states or an
// event) into an Update. Only the fields NeptHass uses are read; everything
// else in the attributes is skipped without being kept.
func ParseState(raw json.RawMessage) (Update, error) {
	var s haState
	if err := json.Unmarshal(raw, &s); err != nil {
		return Update{}, invalid("state object is malformed")
	}
	id, err := entities.ParseID(s.EntityID)
	if err != nil {
		return Update{}, invalid("entity id")
	}
	if !Monitored(string(id.Domain())) {
		return Update{}, errIgnored
	}
	state, ok := asString(s.State)
	if !ok || len(state) > maxStateLen {
		return Update{}, invalid("state value")
	}
	class, _ := asString(s.Attributes.DeviceClass)
	unit, _ := asString(s.Attributes.Unit)
	name, _ := asString(s.Attributes.FriendlyName)
	if len(class) > maxClassLen || len(unit) > maxUnitLen {
		return Update{}, invalid("attribute too long")
	}
	changed, err := parseTime(s.LastChanged)
	if err != nil {
		return Update{}, err
	}
	updated, err := parseTime(s.LastUpdated)
	if err != nil {
		return Update{}, err
	}
	u := Update{ID: id, Name: truncateRunes(name, maxNameRunes), DeviceClass: class, Unit: unit, Raw: state, LastChanged: changed, LastUpdated: updated}
	u.Category, _ = Classify(string(id.Domain()), class)
	return u, nil
}

// ParseEvent validates a state_changed event payload. It returns errIgnored
// for other event types and for entities outside the monitored domains, and
// ErrInvalid for anything malformed, including an entity ID that is not valid.
func ParseEvent(raw json.RawMessage) (Update, error) {
	var ev haEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return Update{}, invalid("event is malformed")
	}
	if ev.EventType != "state_changed" {
		return Update{}, errIgnored
	}
	id, err := entities.ParseID(ev.Data.EntityID)
	if err != nil {
		return Update{}, invalid("entity id")
	}
	if !Monitored(string(id.Domain())) {
		return Update{}, errIgnored
	}
	if isNull(ev.Data.NewState) {
		if isNull(ev.Data.OldState) {
			return Update{}, invalid("removal without a previous state")
		}
		old, err := ParseState(ev.Data.OldState)
		if err != nil {
			return Update{}, err
		}
		if old.ID != id {
			return Update{}, invalid("entity id mismatch")
		}
		return Update{ID: id, Removed: true, PrevUpdated: old.LastUpdated}, nil
	}
	u, err := ParseState(ev.Data.NewState)
	if err != nil {
		return Update{}, err
	}
	if u.ID != id {
		return Update{}, invalid("entity id mismatch")
	}
	return u, nil
}

func isNull(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

// asString decodes a JSON string and reports false for anything else.
func asString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, invalid("timestamp")
	}
	return t.UTC(), nil
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
