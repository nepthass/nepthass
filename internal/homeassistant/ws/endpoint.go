package ws

import (
	"fmt"
	"net/url"
	"strings"
)

// Endpoint derives the WebSocket URL from the configured Home Assistant base
// URL: http becomes ws, https becomes wss, and the API path is appended after
// any path prefix (for example a reverse-proxy sub-path). User info, query and
// fragment are dropped so no credential or tracking parameter can leak.
func Endpoint(base string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", fmt.Errorf("parse home assistant url: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("home assistant url scheme %q is not http or https", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("home assistant url has no host")
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	u.Path = strings.TrimRight(u.Path, "/") + "/api/websocket"
	u.RawPath = ""
	return u.String(), nil
}
