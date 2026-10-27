package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testBotToken = "bot-token-sentinel-0123456789"
	testHAToken  = "ha-token-sentinel-0123456789"
)

func validEnv() map[string]string {
	return map[string]string{
		"DISCORD_BOT_TOKEN":      testBotToken,
		"DISCORD_APPLICATION_ID": "100000000000000001",
		"DISCORD_GUILD_ID":       "100000000000000002",
		"DISCORD_ADMIN_USER_IDS": "100000000000000003",
		"HA_TOKEN":               testHAToken,
	}
}

func lookupOf(env map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestLoad_appliesDefaults_whenOnlyRequiredSettingsAreSet(t *testing.T) {
	cfg, err := Load(lookupOf(validEnv()))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.HomeAssistant.URL != "http://127.0.0.1:8123" {
		t.Errorf("HA URL = %q", cfg.HomeAssistant.URL)
	}
	if cfg.HomeAssistant.Timeout != 10*time.Second {
		t.Errorf("HA timeout = %v", cfg.HomeAssistant.Timeout)
	}
	if cfg.Database.Path != "data/nepthass.db" {
		t.Errorf("database path = %q", cfg.Database.Path)
	}
	if cfg.Database.AuditRetention != 90*24*time.Hour {
		t.Errorf("audit retention = %v", cfg.Database.AuditRetention)
	}
	if cfg.Discord.CommandScope != ScopeGuild {
		t.Errorf("command scope = %q", cfg.Discord.CommandScope)
	}
	if cfg.Log.Level != slog.LevelInfo || cfg.Log.Format != LogText {
		t.Errorf("log = %v/%v", cfg.Log.Level, cfg.Log.Format)
	}
	if cfg.Discord.Token.Reveal() != testBotToken || cfg.HomeAssistant.Token.Reveal() != testHAToken {
		t.Error("tokens were not loaded")
	}
}

func TestLoad_reportsEverySettingProblem_withoutEchoingValues(t *testing.T) {
	env := map[string]string{
		"DISCORD_APPLICATION_ID": "not-a-number-but-secret-looking",
		"DISCORD_GUILD_ID":       "123",
		"DISCORD_ADMIN_USER_IDS": "100000000000000003,oops-value",
		"HA_URL":                 "ftp://user:hunter2@example.test",
		"LOG_LEVEL":              "loud",
	}

	_, err := Load(lookupOf(env))

	if err == nil {
		t.Fatal("Load() succeeded with invalid configuration")
	}
	msg := err.Error()
	for _, key := range []string{
		"DISCORD_BOT_TOKEN", "DISCORD_APPLICATION_ID", "DISCORD_GUILD_ID",
		"DISCORD_ADMIN_USER_IDS", "HA_URL", "HA_TOKEN", "LOG_LEVEL",
	} {
		if !strings.Contains(msg, key) {
			t.Errorf("error does not mention %s:\n%s", key, msg)
		}
	}
	for _, leaked := range []string{"secret-looking", "oops-value", "hunter2", "loud"} {
		if strings.Contains(msg, leaked) {
			t.Errorf("error leaks input value %q:\n%s", leaked, msg)
		}
	}
}

func TestLoad_validatesHomeAssistantURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    string
		wantErr bool
	}{
		{"loopback", "http://127.0.0.1:8123", "http://127.0.0.1:8123", false},
		{"trailing slash is trimmed", "http://homeassistant.local:8123/", "http://homeassistant.local:8123", false},
		{"https with base path", "https://ha.example.test/ha/", "https://ha.example.test/ha", false},
		{"unsupported scheme", "ftp://ha.example.test", "", true},
		{"embedded credentials", "http://user:pass@ha.example.test", "", true},
		{"query string", "http://ha.example.test?x=1", "", true},
		{"missing host", "http://", "", true},
		{"relative", "ha.example.test:8123", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEnv()
			env["HA_URL"] = tt.url

			cfg, err := Load(lookupOf(env))

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() accepted %q", tt.url)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.HomeAssistant.URL != tt.want {
				t.Errorf("URL = %q, want %q", cfg.HomeAssistant.URL, tt.want)
			}
		})
	}
}

func TestLoad_readsSecretFromFile_whenKeyFileIsSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ha_token")
	if err := os.WriteFile(path, []byte(testHAToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := validEnv()
	delete(env, "HA_TOKEN")
	env["HA_TOKEN_FILE"] = path

	cfg, err := Load(lookupOf(env))

	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.HomeAssistant.Token.Reveal(); got != testHAToken {
		t.Errorf("token = %q, want trimmed file contents", got)
	}
}

func TestLoad_rejectsAmbiguousOrBrokenSecretSources(t *testing.T) {
	tests := map[string]func(env map[string]string){
		"both value and file": func(env map[string]string) { env["HA_TOKEN_FILE"] = "/run/secrets/ha" },
		"missing file": func(env map[string]string) {
			delete(env, "HA_TOKEN")
			env["HA_TOKEN_FILE"] = filepath.Join(os.TempDir(), "nepthass-missing-secret")
		},
		"whitespace inside": func(env map[string]string) { env["DISCORD_BOT_TOKEN"] = "Bot abc.def" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			env := validEnv()
			mutate(env)

			if _, err := Load(lookupOf(env)); err == nil {
				t.Fatal("Load() succeeded")
			}
		})
	}
}

func TestLoad_deduplicatesAndSplitsUserLists(t *testing.T) {
	env := validEnv()
	env["DISCORD_ADMIN_USER_IDS"] = "100000000000000003, 100000000000000004 ,100000000000000003"
	env["DISCORD_ALLOWED_USER_IDS"] = "100000000000000005"

	cfg, err := Load(lookupOf(env))

	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := strings.Join(cfg.Discord.AdminUserIDs, ","); got != "100000000000000003,100000000000000004" {
		t.Errorf("admins = %q", got)
	}
	if len(cfg.Discord.AllowedUserIDs) != 1 {
		t.Errorf("allowed = %v", cfg.Discord.AllowedUserIDs)
	}
}

func TestLoad_treatsEmptyValueAsUnset(t *testing.T) {
	env := validEnv()
	env["HA_URL"] = ""
	env["LOG_LEVEL"] = "  "

	cfg, err := Load(lookupOf(env))

	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HomeAssistant.URL != "http://127.0.0.1:8123" || cfg.Log.Level != slog.LevelInfo {
		t.Errorf("defaults not applied: %q %v", cfg.HomeAssistant.URL, cfg.Log.Level)
	}
}

func TestSecret_neverPrintsItsValue(t *testing.T) {
	s := Secret(testBotToken)
	var logBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&logBuf, nil)).Info("check", "token", s)
	jsonBytes, err := json.Marshal(struct{ Token Secret }{s})
	if err != nil {
		t.Fatal(err)
	}

	outputs := []string{
		fmt.Sprint(s), fmt.Sprintf("%v|%+v|%#v|%s|%q|%x", s, s, s, s, s, s),
		logBuf.String(), string(jsonBytes), fmt.Sprintf("%+v", Discord{Token: s}),
	}

	for _, out := range outputs {
		if strings.Contains(out, testBotToken) {
			t.Errorf("secret leaked in %q", out)
		}
	}
}
