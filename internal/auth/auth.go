// Package auth decides which Discord users may use NeptHass at all.
//
// Authorization is explicit application logic. Discord channel or role
// permissions are never consulted: a user is either on the configured
// allowlists in the configured guild or rejected.
package auth

import (
	"errors"

	"github.com/nepthass/nepthass/internal/config"
)

// Level is an ordered privilege level.
type Level int

const (
	// LevelNone is any user that is not on an allowlist.
	LevelNone Level = iota
	// LevelUser may query and control entities it has been granted.
	LevelUser
	// LevelAdmin may additionally manage permissions and read the audit log.
	LevelAdmin
)

var (
	// ErrWrongGuild means the interaction did not come from the configured guild (this includes DMs).
	ErrWrongGuild = errors.New("interaction is not from the configured guild")
	// ErrNotAuthorized means the user is not on any allowlist.
	ErrNotAuthorized = errors.New("user is not authorized")
	// ErrAdminRequired means the action needs administrator access.
	ErrAdminRequired = errors.New("administrator access required")
)

// Actor is an authenticated Discord user and the level they were granted.
type Actor struct {
	UserID string
	Level  Level
}

// Require returns an error unless the actor holds at least the given level.
func (a Actor) Require(minimum Level) error {
	switch {
	case a.Level >= minimum:
		return nil
	case minimum >= LevelAdmin:
		return ErrAdminRequired
	default:
		return ErrNotAuthorized
	}
}

// Authorizer resolves Discord identities to actors.
type Authorizer struct {
	guildID string
	admins  map[string]struct{}
	users   map[string]struct{}
}

// NewAuthorizer builds an Authorizer from the validated Discord configuration.
func NewAuthorizer(cfg config.Discord) *Authorizer {
	set := func(ids []string) map[string]struct{} {
		m := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			m[id] = struct{}{}
		}
		return m
	}
	return &Authorizer{guildID: cfg.GuildID, admins: set(cfg.AdminUserIDs), users: set(cfg.AllowedUserIDs)}
}

// Authorize checks the guild first and then the user allowlists. It must be
// called for every interaction, whatever its type.
func (a *Authorizer) Authorize(guildID, userID string) (Actor, error) {
	if guildID != a.guildID {
		return Actor{}, ErrWrongGuild
	}
	if _, ok := a.admins[userID]; ok {
		return Actor{UserID: userID, Level: LevelAdmin}, nil
	}
	if _, ok := a.users[userID]; ok {
		return Actor{UserID: userID, Level: LevelUser}, nil
	}
	return Actor{}, ErrNotAuthorized
}
