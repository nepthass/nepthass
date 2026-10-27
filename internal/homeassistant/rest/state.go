package rest

import (
	"fmt"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
)

// stateDTO is the wire format of a Home Assistant state object.
type stateDTO struct {
	EntityID    string         `json:"entity_id"`
	State       string         `json:"state"`
	Attributes  map[string]any `json:"attributes"`
	LastChanged time.Time      `json:"last_changed"`
	LastUpdated time.Time      `json:"last_updated"`
}

func (d stateDTO) toState() (entities.State, error) {
	id, err := entities.ParseID(d.EntityID)
	if err != nil {
		return entities.State{}, fmt.Errorf("decode state: %w", err)
	}
	return entities.State{
		ID:          id,
		State:       d.State,
		Attributes:  d.Attributes,
		LastChanged: d.LastChanged,
		LastUpdated: d.LastUpdated,
	}, nil
}
