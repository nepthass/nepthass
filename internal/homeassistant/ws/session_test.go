package ws

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nepthass/nepthass/tests/fakeha"
)

const token = "test-token"

// waitTimeout bounds every wait; nothing here sleeps for a fixed time.
const waitTimeout = 5 * time.Second

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), waitTimeout)
	t.Cleanup(cancel)
	return c
}

func dial(t *testing.T, s *fakeha.Server, mod func(*Options)) (*Session, error) {
	t.Helper()
	endpoint, err := Endpoint(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Endpoint: endpoint, Token: token, HandshakeTimeout: time.Second, PingInterval: time.Hour}
	if mod != nil {
		mod(&opts)
	}
	return Dial(ctx(t), opts)
}

func mustDial(t *testing.T, s *fakeha.Server, mod func(*Options)) *Session {
	t.Helper()
	sess, err := dial(t, s, mod)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(sess.Close)
	return sess
}

func done(t *testing.T, sess *Session) error {
	t.Helper()
	select {
	case <-sess.Done():
		return sess.Err()
	case <-time.After(waitTimeout):
		t.Fatal("the session did not end in time")
		return nil
	}
}

func TestEndpoint(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{"http://homeassistant:8123", "ws://homeassistant:8123/api/websocket", false},
		{"https://ha.example.org", "wss://ha.example.org/api/websocket", false},
		{"http://192.168.1.10:8123/", "ws://192.168.1.10:8123/api/websocket", false},
		{"https://example.org/ha/", "wss://example.org/ha/api/websocket", false},
		{"  http://h:8123  ", "ws://h:8123/api/websocket", false},
		{"http://user:secret@h:8123/?token=abc#frag", "ws://h:8123/api/websocket", false},
		{"HTTP://H:8123", "ws://H:8123/api/websocket", false},
		{"ws://h:8123", "", true},
		{"ftp://h", "", true},
		{"http://", "", true},
		{"homeassistant:8123", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := Endpoint(tt.in)

			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("Endpoint(%q) = %q, %v", tt.in, got, err)
			}
			if err == nil && strings.Contains(got, "secret") {
				t.Errorf("credentials leaked into %q", got)
			}
		})
	}
}

func TestDial_authenticates_andReportsTheLayersInOrder(t *testing.T) {
	s := fakeha.New(t, token)
	var connected atomic.Bool

	sess := mustDial(t, s, func(o *Options) { o.OnConnected = func() { connected.Store(true) } })

	if !connected.Load() || sess.HAVersion() != "2026.10.0" {
		t.Errorf("connected=%v version=%q", connected.Load(), sess.HAVersion())
	}
	if stats := s.WSStats(); stats.AuthSeen != 1 || stats.AuthRejected != 0 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestDial_rejectsAWrongToken_withoutEchoingIt(t *testing.T) {
	s := fakeha.New(t, token)

	_, err := dial(t, s, func(o *Options) { o.Token = "wrong-secret-value" })

	if !errors.Is(err, ErrAuthInvalid) {
		t.Fatalf("error = %v, want ErrAuthInvalid", err)
	}
	if strings.Contains(err.Error(), "wrong-secret-value") {
		t.Errorf("the token appeared in the error: %v", err)
	}
	if s.WSStats().AuthRejected != 1 {
		t.Errorf("stats = %+v", s.WSStats())
	}
}

func TestDial_classifiesBrokenHandshakes(t *testing.T) {
	tests := []struct {
		name string
		mode fakeha.AuthMode
		want error
	}{
		{"invalid", fakeha.AuthInvalid, ErrAuthInvalid},
		{"wrong first message", fakeha.AuthBadFirst, ErrProtocol},
		{"garbage answer", fakeha.AuthGarbage, ErrProtocol},
		{"unexpected answer", fakeha.AuthUnexpectd, ErrProtocol},
		{"never answers", fakeha.AuthSilent, ErrTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeha.New(t, token)
			s.Script(func(sc *fakeha.Script) { sc.Auth = tt.mode })

			_, err := dial(t, s, func(o *Options) { o.HandshakeTimeout = 150 * time.Millisecond })

			if !errors.Is(err, tt.want) {
				t.Errorf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestDial_stopsPromptlyWhenCanceledDuringTheHandshake(t *testing.T) {
	s := fakeha.New(t, token)
	s.Script(func(sc *fakeha.Script) { sc.Auth = fakeha.AuthSilent })
	endpoint, _ := Endpoint(s.URL)
	c, cancel := context.WithCancel(context.Background())
	connected := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := Dial(c, Options{Endpoint: endpoint, Token: token, HandshakeTimeout: time.Minute, OnConnected: func() { close(connected) }})
		result <- err
	}()

	<-connected
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Dial ignored the cancellation")
	}
}

func TestDial_reportsAnUnreachableServerAsATransportFailure(t *testing.T) {
	s := fakeha.New(t, token)
	endpoint, _ := Endpoint(s.URL)
	s.Close()

	_, err := Dial(ctx(t), Options{Endpoint: endpoint, Token: token, HandshakeTimeout: time.Second})

	if !errors.Is(err, ErrTransport) {
		t.Errorf("error = %v, want ErrTransport", err)
	}
}

func TestCall_matchesResultsByID_andReportsRejections(t *testing.T) {
	s := fakeha.New(t, token)
	s.Set("sensor.a", "1", nil)
	sess := mustDial(t, s, nil)
	sess.Start(nil)

	subscribed, subErr := sess.Call(ctx(t), "subscribe_events", map[string]any{"event_type": "state_changed"})
	states, statesErr := sess.Call(ctx(t), "get_states", nil)
	_, unknownErr := sess.Call(ctx(t), "frobnicate", nil)

	var rejected *RejectedError
	if subErr != nil || string(subscribed) != "null" {
		t.Errorf("subscribe = %s, %v", subscribed, subErr)
	}
	var list []map[string]any
	if statesErr != nil || json.Unmarshal(states, &list) != nil || len(list) != 1 || list[0]["entity_id"] != "sensor.a" {
		t.Errorf("get_states = %s, %v", states, statesErr)
	}
	if !errors.As(unknownErr, &rejected) || rejected.Code != "unknown_command" || !errors.Is(unknownErr, ErrCommandRejected) {
		t.Errorf("unknown command error = %v", unknownErr)
	}
}

func TestCall_concurrentCallsEachGetTheirOwnResult(t *testing.T) {
	s := fakeha.New(t, token)
	s.Set("sensor.a", "1", nil)
	sess := mustDial(t, s, nil)
	sess.Start(nil)
	errs := make(chan error, 8)

	for range 8 {
		go func() {
			res, err := sess.Call(ctx(t), "get_states", nil)
			if err == nil && !strings.Contains(string(res), "sensor.a") {
				err = errors.New("wrong result")
			}
			errs <- err
		}()
	}

	for range 8 {
		if err := <-errs; err != nil {
			t.Errorf("concurrent call failed: %v", err)
		}
	}
}

func TestEvents_reachTheHandler_withTheSubscriptionID(t *testing.T) {
	s := fakeha.New(t, token)
	got := make(chan json.RawMessage, 4)
	var subID atomic.Int64
	sess := mustDial(t, s, nil)
	sess.Start(func(id int64, ev json.RawMessage) { subID.Store(id); got <- ev })
	if _, err := sess.Call(ctx(t), "subscribe_events", map[string]any{"event_type": "state_changed"}); err != nil {
		t.Fatal(err)
	}

	s.Update("sensor.a", "5", map[string]any{"device_class": "temperature"})

	select {
	case ev := <-got:
		var payload struct {
			EventType string `json:"event_type"`
			Data      struct {
				EntityID string `json:"entity_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(ev, &payload); err != nil || payload.EventType != "state_changed" || payload.Data.EntityID != "sensor.a" || subID.Load() != 1 {
			t.Errorf("event = %s (%v) subscription=%d", ev, err, subID.Load())
		}
	case <-time.After(waitTimeout):
		t.Fatal("no event delivered")
	}
}

func TestMalformedJSON_endsTheSession_asAProtocolViolation(t *testing.T) {
	s := fakeha.New(t, token)
	sess := mustDial(t, s, nil)
	sess.Start(nil)

	s.SendText("{this is not json")

	if err := done(t, sess); !errors.Is(err, ErrProtocol) {
		t.Errorf("error = %v, want ErrProtocol", err)
	}
	if strings.Contains(sess.Err().Error(), "this is not json") {
		t.Error("the offending bytes were echoed in the error")
	}
}

func TestOversizedMessage_endsTheSession_beforeItIsBuffered(t *testing.T) {
	s := fakeha.New(t, token)
	s.Set("sensor.a", "1", nil)
	s.Script(func(sc *fakeha.Script) { sc.PadSnapshotBytes = 20_000 })
	sess := mustDial(t, s, func(o *Options) { o.MaxMessageBytes = 4096 })
	sess.Start(nil)

	_, err := sess.Call(ctx(t), "get_states", nil)

	if !errors.Is(err, ErrProtocol) {
		t.Errorf("Call error = %v, want ErrProtocol (message over the limit)", err)
	}
	if err := done(t, sess); !errors.Is(err, ErrProtocol) {
		t.Errorf("session error = %v", err)
	}
}

func TestUnexpectedMessages_areCountedAndIgnored(t *testing.T) {
	s := fakeha.New(t, token)
	sess := mustDial(t, s, nil)
	sess.Start(nil)

	s.SendText(`{"type":"mystery"}`)
	s.SendText(`{"id":9999,"type":"result","success":true}`)
	_, err := sess.Call(ctx(t), "get_states", nil) // proves the session is still healthy afterwards

	if err != nil || sess.Unexpected() != 2 {
		t.Errorf("err=%v unexpected=%d, want a live session and 2 unexpected messages", err, sess.Unexpected())
	}
}

func TestPing_aMissingPongEndsTheSession(t *testing.T) {
	s := fakeha.New(t, token)
	s.Script(func(sc *fakeha.Script) { sc.IgnorePing = true })
	sess := mustDial(t, s, func(o *Options) { o.PingInterval = 20 * time.Millisecond; o.PongTimeout = 60 * time.Millisecond })
	sess.Start(nil)

	if err := done(t, sess); !errors.Is(err, ErrTimeout) {
		t.Errorf("error = %v, want ErrTimeout", err)
	}
}

func TestPing_keepsAQuietConnectionAlive(t *testing.T) {
	s := fakeha.New(t, token)
	sess := mustDial(t, s, func(o *Options) {
		o.PingInterval = 15 * time.Millisecond
		o.ReadTimeout = 200 * time.Millisecond
	})
	sess.Start(nil)

	if err := s.WaitWS(ctx(t), func(st fakeha.WSStats) bool { return st.Pings >= 5 }); err != nil {
		t.Fatalf("pings were not sent: %v", err)
	}

	select {
	case <-sess.Done():
		t.Errorf("a quiet but pinged connection ended: %v", sess.Err())
	default:
	}
}

func TestReadTimeout_endsASilentConnection(t *testing.T) {
	s := fakeha.New(t, token)
	sess := mustDial(t, s, func(o *Options) { o.ReadTimeout = 50 * time.Millisecond })
	sess.Start(nil)

	if err := done(t, sess); !errors.Is(err, ErrTimeout) {
		t.Errorf("error = %v, want ErrTimeout", err)
	}
}

func TestServerClose_isATransportFailure(t *testing.T) {
	s := fakeha.New(t, token)
	sess := mustDial(t, s, nil)
	sess.Start(nil)

	s.DisconnectAll(true)

	if err := done(t, sess); !errors.Is(err, ErrTransport) {
		t.Errorf("error = %v, want ErrTransport", err)
	}
}

func TestClose_isIdempotent_failsPendingCalls_andReleasesTheSocket(t *testing.T) {
	s := fakeha.New(t, token)
	s.Script(func(sc *fakeha.Script) { sc.HoldStates = true })
	t.Cleanup(s.ReleaseStates)
	sess := mustDial(t, s, nil)
	sess.Start(nil)
	pending := make(chan error, 1)
	go func() { _, err := sess.Call(ctx(t), "get_states", nil); pending <- err }()
	if err := s.WaitWS(ctx(t), func(st fakeha.WSStats) bool { return st.StateRequests == 1 }); err != nil {
		t.Fatal(err)
	}

	finished := make(chan struct{})
	go func() { sess.Close(); sess.Close(); close(finished) }()

	select {
	case <-finished:
	case <-time.After(waitTimeout):
		t.Fatal("Close did not return: a goroutine is stuck")
	}
	if err := <-pending; !errors.Is(err, ErrClosed) {
		t.Errorf("the pending call ended with %v, want ErrClosed", err)
	}
	if _, err := sess.Call(ctx(t), "get_states", nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Call after Close = %v, want ErrClosed", err)
	}
	s.ReleaseStates() // the fake's handler is parked on the gate and cannot notice the close until released
	if err := s.WaitWS(ctx(t), func(st fakeha.WSStats) bool { return st.Active == 0 }); err != nil {
		t.Errorf("the server still sees an open connection: %v", err)
	}
}
