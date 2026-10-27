package entities

import (
	"context"
	"strings"

	"github.com/nepthass/nepthass/internal/auth"
)

// Resolve turns user input into a canonical entity ID. A valid entity ID wins;
// otherwise the input is treated as an alias. An alias resolves only to an
// entity the caller already holds at least the required access on, and anything
// else behaves exactly like an unknown name (ErrInvalidID), so a stranger
// cannot learn which aliases exist. Resolution is deterministic because
// aliases are unique and can never look like an entity ID.
func (s *Service) Resolve(ctx context.Context, actor auth.Actor, input string, minimum Access) (ID, error) {
	if err := actor.Require(auth.LevelUser); err != nil {
		return "", err
	}
	input = strings.TrimSpace(input)
	if id, err := ParseID(input); err == nil {
		return id, nil
	}
	_, key, err := NormalizeAlias(input)
	if err != nil {
		return "", ErrInvalidID
	}
	holder, found, err := s.d.Aliases.Lookup(ctx, key)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrInvalidID
	}
	access, err := s.d.Permissions.Get(ctx, holder)
	if err != nil {
		return "", err
	}
	if !access.AtLeast(minimum) {
		return "", ErrInvalidID
	}
	return holder, nil
}
