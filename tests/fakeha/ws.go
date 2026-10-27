package fakeha

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// AuthMode scripts how the WebSocket endpoint answers the auth message.
type AuthMode string

const (
	AuthOK        AuthMode = "ok"
	AuthInvalid   AuthMode = "invalid"   // answers auth_invalid
	AuthSilent    AuthMode = "silent"    // never answers
	AuthBadFirst  AuthMode = "bad-first" // sends something other than auth_required first
	AuthGarbage   AuthMode = "garbage"   // answers with invalid JSON
	AuthUnexpectd AuthMode = "unexpected"
)

// Script is the mutable behavior of the WebSocket endpoint. Change it with
// Server.Script while a test runs.
type Script struct {
	Auth AuthMode
	// RejectSubscribe answers subscribe_events with success=false.
	RejectSubscribe bool
	// SilentSubscribe never answers subscribe_events.
	SilentSubscribe bool
	// HoldStates makes get_states wait for ReleaseStates after capturing the
	// snapshot, so tests can inject events between the snapshot and its delivery.
	HoldStates bool
	// IgnorePing never answers ping.
	IgnorePing bool
	// PadSnapshotBytes inflates the get_states response (oversize tests).
	PadSnapshotBytes int
}

// WSStats are connection counters.
type WSStats struct {
	Connections   int // accepted upgrades since start
	Active        int // currently open
	Subscribed    int // connections currently holding a state_changed subscription
	MaxSubscribed int // highest value Subscribed ever reached
	Pings         int
	StateRequests int
	AuthRejected  int
	AuthSeen      int // auth messages received
}

type wsState struct {
	mu      sync.Mutex
	script  Script
	conns   map[*wsConn]struct{}
	stats   WSStats
	down    bool
	changed chan struct{}
	gate    chan struct{}
}

func (w *wsState) init() {
	w.script = Script{Auth: AuthOK}
	w.conns = map[*wsConn]struct{}{}
	w.changed = make(chan struct{})
	w.gate = make(chan struct{})
}

// touch wakes waiters after a stats change. The caller holds w.mu.
func (w *wsState) touch() {
	close(w.changed)
	w.changed = make(chan struct{})
}

type wsConn struct {
	c     *websocket.Conn
	wmu   sync.Mutex
	subID int64
}

func (c *wsConn) send(v any) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.c.WriteJSON(v)
}

func (c *wsConn) sendRaw(typ int, data []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.c.WriteMessage(typ, data)
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// Script modifies the endpoint's behavior under lock.
func (s *Server) Script(f func(*Script)) {
	s.ws.mu.Lock()
	defer s.ws.mu.Unlock()
	f(&s.ws.script)
}

// WSStats returns a copy of the counters.
func (s *Server) WSStats() WSStats {
	s.ws.mu.Lock()
	defer s.ws.mu.Unlock()
	return s.ws.stats
}

// WaitWS blocks until pred(stats) holds or ctx ends. It reacts to changes; it
// does not poll.
func (s *Server) WaitWS(ctx context.Context, pred func(WSStats) bool) error {
	for {
		s.ws.mu.Lock()
		ok, ch := pred(s.ws.stats), s.ws.changed
		s.ws.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Down simulates a Home Assistant restart: open connections are dropped and
// new upgrades are refused with 503 until Up.
func (s *Server) Down() {
	s.ws.mu.Lock()
	s.ws.down = true
	conns := s.connsLocked()
	s.ws.mu.Unlock()
	for _, c := range conns {
		_ = c.c.Close()
	}
}

// Up ends Down.
func (s *Server) Up() {
	s.ws.mu.Lock()
	s.ws.down = false
	s.ws.touch()
	s.ws.mu.Unlock()
}

// DisconnectAll closes every WebSocket connection. clean sends a going-away
// close frame first; otherwise the socket is dropped abruptly.
func (s *Server) DisconnectAll(clean bool) {
	s.ws.mu.Lock()
	conns := s.connsLocked()
	s.ws.mu.Unlock()
	for _, c := range conns {
		if clean {
			c.wmu.Lock()
			_ = c.c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, ""), time.Now().Add(time.Second))
			c.wmu.Unlock()
		}
		_ = c.c.Close()
	}
}

func (s *Server) connsLocked() []*wsConn {
	out := make([]*wsConn, 0, len(s.ws.conns))
	for c := range s.ws.conns {
		out = append(out, c)
	}
	return out
}

// ReleaseStates lets a held get_states answer proceed.
func (s *Server) ReleaseStates() {
	s.ws.mu.Lock()
	close(s.ws.gate)
	s.ws.gate = make(chan struct{})
	s.ws.mu.Unlock()
}

// Update changes an entity the way Home Assistant does: a new state value
// moves last_changed and last_updated, an attribute-only change moves only
// last_updated. It then emits a state_changed event to subscribers.
func (s *Server) Update(id, state string, attrs map[string]any) {
	s.mu.Lock()
	old, existed := s.entities[id]
	now := s.clockLocked()
	next := entity{state: state, attrs: attrs, changed: now, updated: now}
	if existed && old.state == state {
		next.changed = old.changed
	}
	s.entities[id] = next
	var oldObj any
	if existed {
		oldObj = StateObject(id, old.state, old.attrs, old.changed, old.updated)
	}
	newObj := s.render(id)
	s.mu.Unlock()
	s.EmitStateChanged(id, oldObj, newObj)
}

// Remove deletes an entity and emits a removal event (new_state null).
func (s *Server) Remove(id string) {
	s.mu.Lock()
	old, existed := s.entities[id]
	delete(s.entities, id)
	s.mu.Unlock()
	if existed {
		s.EmitStateChanged(id, StateObject(id, old.state, old.attrs, old.changed, old.updated), nil)
	}
}

// EmitStateChanged sends a state_changed event with arbitrary old and new
// state objects to every subscribed connection (use nil for a missing state).
func (s *Server) EmitStateChanged(id string, oldState, newState any) {
	s.broadcast(func(subID int64) any {
		return map[string]any{"id": subID, "type": "event", "event": map[string]any{
			"event_type": "state_changed", "origin": "LOCAL", "time_fired": time.Now().UTC().Format(time.RFC3339Nano),
			"data": map[string]any{"entity_id": id, "old_state": oldState, "new_state": newState},
		}}
	})
}

// EmitRawEvent sends an arbitrary event payload under the subscription id.
func (s *Server) EmitRawEvent(event any) {
	s.broadcast(func(subID int64) any { return map[string]any{"id": subID, "type": "event", "event": event} })
}

// SendText sends a raw text frame to every connection (malformed JSON tests).
func (s *Server) SendText(data string) {
	s.ws.mu.Lock()
	conns := s.connsLocked()
	s.ws.mu.Unlock()
	for _, c := range conns {
		_ = c.sendRaw(websocket.TextMessage, []byte(data))
	}
}

func (s *Server) broadcast(build func(subID int64) any) {
	s.ws.mu.Lock()
	var targets []*wsConn
	for c := range s.ws.conns {
		if c.subID != 0 {
			targets = append(targets, c)
		}
	}
	s.ws.mu.Unlock()
	for _, c := range targets {
		_ = c.send(build(c.subID))
	}
}

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	s.ws.mu.Lock()
	if s.ws.down {
		s.ws.mu.Unlock()
		http.Error(w, "restarting", http.StatusServiceUnavailable)
		return
	}
	s.ws.mu.Unlock()
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &wsConn{c: conn}
	s.ws.mu.Lock()
	s.ws.conns[c] = struct{}{}
	s.ws.stats.Connections++
	s.ws.stats.Active++
	s.ws.touch()
	s.ws.mu.Unlock()
	defer s.dropConn(c)
	s.converse(c)
}

func (s *Server) dropConn(c *wsConn) {
	_ = c.c.Close()
	s.ws.mu.Lock()
	defer s.ws.mu.Unlock()
	if _, ok := s.ws.conns[c]; !ok {
		return
	}
	delete(s.ws.conns, c)
	s.ws.stats.Active--
	if c.subID != 0 {
		s.ws.stats.Subscribed--
		c.subID = 0
	}
	s.ws.touch()
}

type clientMsg struct {
	ID          int64  `json:"id"`
	Type        string `json:"type"`
	AccessToken string `json:"access_token"`
	EventType   string `json:"event_type"`
}

func (s *Server) converse(c *wsConn) {
	script := s.snapshotScript()
	first := map[string]any{"type": "auth_required", "ha_version": "2026.10.0"}
	if script.Auth == AuthBadFirst {
		first = map[string]any{"type": "hello"}
	}
	if c.send(first) != nil || script.Auth == AuthBadFirst {
		return
	}
	var auth clientMsg
	if err := c.c.ReadJSON(&auth); err != nil || auth.Type != "auth" {
		return
	}
	s.ws.mu.Lock()
	s.ws.stats.AuthSeen++
	s.ws.touch()
	s.ws.mu.Unlock()
	script = s.snapshotScript()
	switch {
	case script.Auth == AuthSilent:
		_, _, _ = c.c.ReadMessage()
		return
	case script.Auth == AuthGarbage:
		_ = c.sendRaw(websocket.TextMessage, []byte("{not json"))
		return
	case script.Auth == AuthUnexpectd:
		_ = c.send(map[string]any{"type": "result", "id": 1})
		return
	case script.Auth == AuthInvalid || subtle.ConstantTimeCompare([]byte(auth.AccessToken), []byte(s.token)) != 1:
		s.ws.mu.Lock()
		s.ws.stats.AuthRejected++
		s.ws.touch()
		s.ws.mu.Unlock()
		_ = c.send(map[string]any{"type": "auth_invalid", "message": "Invalid access token or password"})
		return
	}
	if c.send(map[string]any{"type": "auth_ok", "ha_version": "2026.10.0"}) != nil {
		return
	}
	for {
		var m clientMsg
		if err := c.c.ReadJSON(&m); err != nil {
			return
		}
		s.handleCommand(c, m)
	}
}

func (s *Server) snapshotScript() Script {
	s.ws.mu.Lock()
	defer s.ws.mu.Unlock()
	return s.ws.script
}

func (s *Server) handleCommand(c *wsConn, m clientMsg) {
	script := s.snapshotScript()
	switch m.Type {
	case "ping":
		s.ws.mu.Lock()
		s.ws.stats.Pings++
		s.ws.touch()
		s.ws.mu.Unlock()
		if !script.IgnorePing {
			_ = c.send(map[string]any{"id": m.ID, "type": "pong"})
		}
	case "subscribe_events":
		switch {
		case script.SilentSubscribe:
		case script.RejectSubscribe || m.EventType != "state_changed":
			_ = c.send(map[string]any{"id": m.ID, "type": "result", "success": false, "error": map[string]any{"code": "not_allowed", "message": "rejected"}})
		default:
			s.ws.mu.Lock()
			if c.subID == 0 {
				s.ws.stats.Subscribed++
				s.ws.stats.MaxSubscribed = max(s.ws.stats.MaxSubscribed, s.ws.stats.Subscribed)
			}
			c.subID = m.ID
			s.ws.touch()
			s.ws.mu.Unlock()
			_ = c.send(map[string]any{"id": m.ID, "type": "result", "success": true, "result": nil})
		}
	case "get_states":
		s.handleGetStates(c, m, script)
	default:
		_ = c.send(map[string]any{"id": m.ID, "type": "result", "success": false, "error": map[string]any{"code": "unknown_command", "message": "Unknown command."}})
	}
}

func (s *Server) handleGetStates(c *wsConn, m clientMsg, script Script) {
	s.mu.Lock()
	snapshot := make([]map[string]any, 0, len(s.entities))
	for id := range s.entities {
		snapshot = append(snapshot, s.render(id))
	}
	s.mu.Unlock()
	if script.PadSnapshotBytes > 0 && len(snapshot) > 0 {
		snapshot[0]["attributes"] = map[string]any{"padding": strings.Repeat("x", script.PadSnapshotBytes)}
	}
	s.ws.mu.Lock()
	s.ws.stats.StateRequests++
	gate := s.ws.gate
	s.ws.touch()
	s.ws.mu.Unlock()
	if script.HoldStates {
		<-gate
	}
	body, _ := json.Marshal(map[string]any{"id": m.ID, "type": "result", "success": true, "result": snapshot})
	_ = c.sendRaw(websocket.TextMessage, body)
}
