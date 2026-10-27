// Package config loads and validates NeptHass bootstrap configuration.
//
// Bootstrap settings (credentials, IDs, paths) come from the environment or
// from *_FILE secret files. They are never accepted through Discord.
package config

import (
	"log/slog"
	"time"
)

// CommandScope selects where slash commands are registered.
type CommandScope string

const (
	// ScopeGuild registers commands in the configured guild only (instant, for development and small homes).
	ScopeGuild CommandScope = "guild"
	// ScopeGlobal registers commands globally (slow to propagate, intended for releases).
	ScopeGlobal CommandScope = "global"
)

// LogFormat selects the slog output encoding.
type LogFormat string

const (
	LogText LogFormat = "text"
	LogJSON LogFormat = "json"
)

const (
	defaultHAURL        = "http://127.0.0.1:8123"
	defaultDatabasePath = "data/nepthass.db"
	defaultHATimeout    = 10 * time.Second
	defaultAuditDays    = 90
	maxAuditDays        = 3650
	minHATimeout        = time.Second
	maxHATimeout        = 2 * time.Minute
	hoursPerDay         = 24 * time.Hour
)

// Config is the validated, immutable application configuration.
type Config struct {
	Discord       Discord
	HomeAssistant HomeAssistant
	Database      Database
	Log           Log
}

// Discord holds the bot credentials and the authorization allowlists.
type Discord struct {
	Token          Secret
	ApplicationID  string
	GuildID        string
	AdminUserIDs   []string
	AllowedUserIDs []string
	CommandScope   CommandScope
}

// HomeAssistant holds the Home Assistant connection settings.
type HomeAssistant struct {
	URL     string
	Token   Secret
	Timeout time.Duration
}

// Database holds the SQLite settings.
type Database struct {
	Path           string
	AuditRetention time.Duration
}

// Log holds the logging settings.
type Log struct {
	Level  slog.Level
	Format LogFormat
}

// Load reads and validates the configuration. On failure it returns an error
// listing every invalid setting; the error never contains secret values.
func Load(lookup LookupFunc) (*Config, error) {
	l := &loader{lookup: lookup}
	cfg := &Config{
		Discord: Discord{
			Token:          l.secret("DISCORD_BOT_TOKEN"),
			ApplicationID:  l.snowflake("DISCORD_APPLICATION_ID"),
			GuildID:        l.snowflake("DISCORD_GUILD_ID"),
			AdminUserIDs:   l.snowflakeList("DISCORD_ADMIN_USER_IDS", true),
			AllowedUserIDs: l.snowflakeList("DISCORD_ALLOWED_USER_IDS", false),
			CommandScope:   l.scope("DISCORD_COMMAND_SCOPE"),
		},
		HomeAssistant: HomeAssistant{
			URL:     l.baseURL("HA_URL", defaultHAURL),
			Token:   l.secret("HA_TOKEN"),
			Timeout: l.duration("HA_REQUEST_TIMEOUT", defaultHATimeout, minHATimeout, maxHATimeout),
		},
		Database: Database{
			Path:           l.path("DATABASE_PATH", defaultDatabasePath),
			AuditRetention: time.Duration(l.days("AUDIT_RETENTION_DAYS", defaultAuditDays, maxAuditDays)) * hoursPerDay,
		},
		Log: Log{
			Level:  l.logLevel("LOG_LEVEL"),
			Format: l.logFormat("LOG_FORMAT"),
		},
	}
	if err := l.err(); err != nil {
		return nil, err
	}
	return cfg, nil
}
