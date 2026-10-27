//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

// wsCommand sends one authenticated WebSocket command. The REST API cannot
// change the unit system (the UI uses WebSocket for it), and a regression test
// needs to flip it the way the onboarding UI can.
func wsCommand(ctx context.Context, command map[string]any) error {
	u, err := url.Parse(target.url)
	if err != nil {
		return err
	}
	scheme := "ws"
	if u.Scheme == "https" {
		scheme = "wss"
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, scheme+"://"+u.Host+"/api/websocket", nil)
	if err != nil {
		return fmt.Errorf("connect websocket: %w", err)
	}
	defer conn.Close()
	read := func() (map[string]any, error) {
		var msg map[string]any
		if err := conn.SetReadDeadline(deadlineOf(ctx)); err != nil {
			return nil, err
		}
		return msg, conn.ReadJSON(&msg)
	}
	if msg, err := read(); err != nil || msg["type"] != "auth_required" {
		return fmt.Errorf("websocket handshake: %v %v", msg, err)
	}
	if err := conn.WriteJSON(map[string]any{"type": "auth", "access_token": target.token}); err != nil {
		return err
	}
	if msg, err := read(); err != nil || msg["type"] != "auth_ok" {
		return fmt.Errorf("websocket authentication: %v %v", msg, err)
	}
	command["id"] = 1
	if err := conn.WriteJSON(command); err != nil {
		return err
	}
	msg, err := read()
	if err != nil {
		return err
	}
	if ok, _ := msg["success"].(bool); !ok {
		raw, _ := json.Marshal(msg["error"])
		return fmt.Errorf("websocket command failed: %s", raw)
	}
	return nil
}

func deadlineOf(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Now().Add(requestTimeout)
}
