package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// LookupFunc reads one configuration value, like os.LookupEnv.
type LookupFunc func(key string) (value string, ok bool)

// FieldError describes one invalid setting. It never contains the offending
// value because a mistyped setting may well be a secret.
type FieldError struct {
	Key    string
	Reason string
}

func (e *FieldError) Error() string { return e.Key + ": " + e.Reason }

// loader reads settings and collects every problem so that the operator sees
// all of them at once instead of fixing them one restart at a time.
type loader struct {
	lookup LookupFunc
	errs   []error
}

func (l *loader) fail(key, reason string) {
	l.errs = append(l.errs, &FieldError{Key: key, Reason: reason})
}

func (l *loader) err() error {
	if len(l.errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid configuration:\n%w", errors.Join(l.errs...))
}

// get returns the trimmed value; an empty value counts as unset so that
// "KEY=" in an env file behaves like a missing key.
func (l *loader) get(key string) (string, bool) {
	v, ok := l.lookup(key)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

func (l *loader) str(key, fallback string) string {
	if v, ok := l.get(key); ok {
		return v
	}
	return fallback
}

// secret reads KEY, or the contents of the file named by KEY_FILE.
func (l *loader) secret(key string) Secret {
	direct, hasDirect := l.get(key)
	path, hasFile := l.get(key + "_FILE")
	switch {
	case hasDirect && hasFile:
		l.fail(key, "set either "+key+" or "+key+"_FILE, not both")
		return ""
	case hasFile:
		raw, err := os.ReadFile(path)
		if err != nil {
			l.fail(key+"_FILE", "cannot read secret file")
			return ""
		}
		direct = strings.TrimSpace(string(raw))
	case !hasDirect:
		l.fail(key, "required (or set "+key+"_FILE)")
		return ""
	}
	if direct == "" || strings.ContainsAny(direct, " \t\r\n") {
		l.fail(key, "must be a single token without whitespace or a \"Bot \" prefix")
		return ""
	}
	return Secret(direct)
}

func validSnowflake(v string) bool {
	if len(v) < 17 || len(v) > 20 {
		return false
	}
	_, err := strconv.ParseUint(v, 10, 64)
	return err == nil
}

func (l *loader) snowflake(key string) string {
	v, ok := l.get(key)
	if !ok {
		l.fail(key, "required")
		return ""
	}
	if !validSnowflake(v) {
		l.fail(key, "must be a numeric Discord ID (17-20 digits)")
		return ""
	}
	return v
}

// snowflakeList parses a comma-separated list of Discord IDs, dropping duplicates.
func (l *loader) snowflakeList(key string, required bool) []string {
	raw, ok := l.get(key)
	if !ok {
		if required {
			l.fail(key, "required (comma-separated Discord user IDs)")
		}
		return nil
	}
	seen := make(map[string]struct{})
	var ids []string
	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if !validSnowflake(id) {
			l.fail(key, "every entry must be a numeric Discord ID (17-20 digits)")
			return nil
		}
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	return ids
}

func (l *loader) scope(key string) CommandScope {
	switch v := CommandScope(strings.ToLower(l.str(key, string(ScopeGuild)))); v {
	case ScopeGuild, ScopeGlobal:
		return v
	default:
		l.fail(key, "must be \"guild\" or \"global\"")
		return ScopeGuild
	}
}

func (l *loader) baseURL(key, fallback string) string {
	u, err := url.Parse(l.str(key, fallback))
	switch {
	case err != nil || u.Hostname() == "":
		l.fail(key, "must be an absolute http(s) URL such as http://127.0.0.1:8123")
	case u.Scheme != "http" && u.Scheme != "https":
		l.fail(key, "scheme must be http or https")
	case u.User != nil:
		l.fail(key, "must not embed credentials; use HA_TOKEN")
	case u.RawQuery != "" || u.Fragment != "":
		l.fail(key, "must not contain a query or fragment")
	default:
		u.Path = strings.TrimRight(u.Path, "/")
		u.RawPath = ""
		return u.String()
	}
	return fallback
}

func (l *loader) duration(key string, fallback, minimum, maximum time.Duration) time.Duration {
	v, ok := l.get(key)
	if !ok {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < minimum || d > maximum {
		l.fail(key, fmt.Sprintf("must be a duration between %s and %s (for example 10s)", minimum, maximum))
		return fallback
	}
	return d
}

func (l *loader) days(key string, fallback, maximum int) int {
	v, ok := l.get(key)
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maximum {
		l.fail(key, fmt.Sprintf("must be a whole number of days between 1 and %d", maximum))
		return fallback
	}
	return n
}

func (l *loader) logLevel(key string) slog.Level {
	switch strings.ToLower(l.str(key, "info")) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		l.fail(key, "must be one of debug, info, warn, error")
		return slog.LevelInfo
	}
}

func (l *loader) logFormat(key string) LogFormat {
	switch v := LogFormat(strings.ToLower(l.str(key, string(LogText)))); v {
	case LogText, LogJSON:
		return v
	default:
		l.fail(key, "must be \"text\" or \"json\"")
		return LogText
	}
}

func (l *loader) path(key, fallback string) string {
	v := l.str(key, fallback)
	if strings.ContainsRune(v, 0) {
		l.fail(key, "contains an invalid character")
		return fallback
	}
	return v
}
