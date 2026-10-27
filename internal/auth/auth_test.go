package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/config"
)

const (
	guild    = "200000000000000001"
	admin    = "200000000000000002"
	user     = "200000000000000003"
	outsider = "200000000000000004"
)

func testAuthorizer() *Authorizer {
	return NewAuthorizer(config.Discord{GuildID: guild, AdminUserIDs: []string{admin}, AllowedUserIDs: []string{user}})
}

func TestAuthorize(t *testing.T) {
	tests := []struct {
		name      string
		guild     string
		user      string
		wantLevel Level
		wantErr   error
	}{
		{"admin in guild", guild, admin, LevelAdmin, nil},
		{"user in guild", guild, user, LevelUser, nil},
		{"unlisted user in guild", guild, outsider, LevelNone, ErrNotAuthorized},
		{"admin in another guild", "999999999999999999", admin, LevelNone, ErrWrongGuild},
		{"admin in direct message", "", admin, LevelNone, ErrWrongGuild},
		{"empty user", guild, "", LevelNone, ErrNotAuthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actor, err := testAuthorizer().Authorize(tt.guild, tt.user)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if actor.Level != tt.wantLevel {
				t.Errorf("level = %v, want %v", actor.Level, tt.wantLevel)
			}
		})
	}
}

func TestActorRequire(t *testing.T) {
	tests := []struct {
		actor   Level
		need    Level
		wantErr error
	}{
		{LevelAdmin, LevelAdmin, nil},
		{LevelAdmin, LevelUser, nil},
		{LevelUser, LevelUser, nil},
		{LevelUser, LevelAdmin, ErrAdminRequired},
		{LevelNone, LevelUser, ErrNotAuthorized},
		{LevelNone, LevelAdmin, ErrAdminRequired},
	}
	for _, tt := range tests {
		if err := (Actor{Level: tt.actor}).Require(tt.need); !errors.Is(err, tt.wantErr) {
			t.Errorf("Actor(%v).Require(%v) = %v, want %v", tt.actor, tt.need, err, tt.wantErr)
		}
	}
}

func TestLimiter_allowsBurstThenBlocks_andRecoversAfterWindow(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	l := NewLimiter(2, 10*time.Second, func() time.Time { return now })

	first, second, third := l.Allow(user), l.Allow(user), l.Allow(user)
	otherUser := l.Allow(admin)
	now = now.Add(10*time.Second + time.Nanosecond)
	afterWindow := l.Allow(user)

	if !first || !second || third {
		t.Errorf("burst = %v %v %v, want true true false", first, second, third)
	}
	if !otherUser {
		t.Error("limit must be tracked per user")
	}
	if !afterWindow {
		t.Error("user should be allowed again once the window passed")
	}
}
