package rest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/config"
	"github.com/nepthass/nepthass/internal/entities"
)

const testToken = "ha-token-sentinel-abcdef"

func newClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(config.HomeAssistant{URL: srv.URL, Token: testToken, Timeout: 2 * time.Second}), srv
}

func mustID(t *testing.T, s string) entities.ID {
	t.Helper()
	id, err := entities.ParseID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestState_parsesEntity_andSendsBearerToken(t *testing.T) {
	var gotAuth, gotPath string
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		io.WriteString(w, `{"entity_id":"light.kitchen","state":"on",
			"attributes":{"friendly_name":"Kitchen Light","brightness":180},
			"last_changed":"2026-10-27T10:00:00.123456+00:00","last_updated":"2026-10-27T10:00:01+00:00"}`)
	}))

	state, err := c.State(context.Background(), mustID(t, "light.kitchen"))

	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	if gotAuth != "Bearer "+testToken || gotPath != "/api/states/light.kitchen" {
		t.Errorf("request auth=%q path=%q", gotAuth, gotPath)
	}
	if state.ID != "light.kitchen" || state.State != "on" || state.FriendlyName() != "Kitchen Light" {
		t.Errorf("state = %+v", state)
	}
	if state.LastChanged.Year() != 2026 || state.LastChanged.IsZero() {
		t.Errorf("LastChanged = %v", state.LastChanged)
	}
}

func TestStates_skipsEntriesWithMalformedIDs(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"entity_id":"light.a","state":"on"},{"entity_id":"../bad","state":"on"},{"entity_id":"switch.b","state":"unavailable"}]`)
	}))

	states, err := c.States(context.Background())

	if err != nil {
		t.Fatalf("States() error = %v", err)
	}
	if len(states) != 2 || states[0].ID != "light.a" || states[1].Reliable() {
		t.Errorf("states = %+v", states)
	}
}

func TestClient_mapsHTTPFailuresToTypedErrors_withoutLeakingTheToken(t *testing.T) {
	tests := []struct {
		name   string
		status int
		check  func(error) bool
	}{
		{"unauthorized", http.StatusUnauthorized, func(err error) bool { return errors.Is(err, ErrUnauthorized) }},
		{"forbidden", http.StatusForbidden, func(err error) bool { return errors.Is(err, ErrUnauthorized) }},
		{"not found", http.StatusNotFound, func(err error) bool { return errors.Is(err, ErrNotFound) }},
		{"server error", http.StatusInternalServerError, func(err error) bool {
			var api *APIError
			return errors.As(err, &api) && api.Status == 500
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom "+r.Header.Get("Authorization"), tt.status)
			}))

			_, err := c.State(context.Background(), mustID(t, "light.kitchen"))

			if err == nil || !tt.check(err) {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Errorf("error leaks token: %v", err)
			}
		})
	}
}

func TestClient_reportsUnavailable_whenServerIsTooSlow(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	c := New(config.HomeAssistant{URL: srv.URL, Token: testToken, Timeout: 50 * time.Millisecond})

	_, err := c.Info(context.Background())

	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("error = %v, want ErrUnavailable", err)
	}
}

func TestClient_reportsUnavailable_whenServerIsDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := New(config.HomeAssistant{URL: url, Token: testToken, Timeout: time.Second})

	_, err := c.Info(context.Background())

	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("error = %v, want ErrUnavailable", err)
	}
}

func TestClient_doesNotFollowRedirects_soTheTokenCannotBeForwarded(t *testing.T) {
	var otherHits atomic.Int32
	var otherAuth atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		otherAuth.Store(r.Header.Get("Authorization"))
	}))
	t.Cleanup(other.Close)
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))

	_, err := c.Info(context.Background())

	var api *APIError
	if !errors.As(err, &api) || api.Status != http.StatusFound {
		t.Errorf("error = %v, want APIError 302", err)
	}
	if otherHits.Load() != 0 {
		t.Errorf("redirect target was contacted (auth=%v)", otherAuth.Load())
	}
}

func TestControl_callsAllowlistedService(t *testing.T) {
	type call struct{ method, path, contentType, body string }
	var got call
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = call{r.Method, r.URL.Path, r.Header.Get("Content-Type"), string(body)}
		io.WriteString(w, "[]")
	}))

	tests := []struct {
		id     string
		action entities.Action
		path   string
	}{
		{"light.kitchen", entities.ActionOn, "/api/services/light/turn_on"},
		{"switch.fan", entities.ActionOff, "/api/services/switch/turn_off"},
	}
	for _, tt := range tests {
		got = call{}
		if err := c.Control(context.Background(), mustID(t, tt.id), tt.action); err != nil {
			t.Fatalf("Control(%s) error = %v", tt.id, err)
		}
		var payload map[string]string
		if err := json.Unmarshal([]byte(got.body), &payload); err != nil {
			t.Fatal(err)
		}
		if got.method != http.MethodPost || got.path != tt.path || got.contentType != "application/json" ||
			len(payload) != 1 || payload["entity_id"] != tt.id {
			t.Errorf("request = %+v", got)
		}
	}
}

func TestControl_refusesEveryDomainOutsideTheAllowlist_withoutNetworkTraffic(t *testing.T) {
	var hits atomic.Int32
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))

	for _, id := range []string{
		"lock.front_door", "cover.garage_door", "alarm_control_panel.home",
		"climate.hall", "siren.alarm", "script.anything", "homeassistant.main",
	} {
		for _, action := range []entities.Action{entities.ActionOn, entities.ActionOff} {
			err := c.Control(context.Background(), mustID(t, id), action)

			if !errors.Is(err, ErrServiceNotAllowed) {
				t.Errorf("Control(%s,%s) error = %v, want ErrServiceNotAllowed", id, action, err)
			}
		}
	}
	if hits.Load() != 0 {
		t.Errorf("server received %d requests, want 0", hits.Load())
	}
}

func TestClient_honoursBasePath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		io.WriteString(w, `{"version":"2026.10.1"}`)
	}))
	t.Cleanup(srv.Close)
	c := New(config.HomeAssistant{URL: srv.URL + "/ha", Token: testToken, Timeout: time.Second})

	info, err := c.Info(context.Background())

	if err != nil || info.Version != "2026.10.1" || gotPath != "/ha/api/config" {
		t.Errorf("info=%+v err=%v path=%q", info, err, gotPath)
	}
}
