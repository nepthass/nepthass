package commands_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/homeassistant/ws"
	"github.com/nepthass/nepthass/internal/monitor"
	"github.com/nepthass/nepthass/tests/fakeha"
)

func TestMonitoringText_everyConnectionStateHasAnAccurateMessage(t *testing.T) {
	retry := time.Unix(1793310000, 0)
	tests := []struct {
		name    string
		st      monitor.Status
		want    []string
		notWant []string
	}{
		{"not started", monitor.Status{State: monitor.StateDisconnected}, []string{"Not started yet"}, []string{"Starting"}},
		{"first start: connecting", monitor.Status{State: monitor.StateConnecting}, []string{"Starting (connecting)"}, []string{"Reconnecting"}},
		{"first start: authenticating", monitor.Status{State: monitor.StateAuthenticating}, []string{"Starting (authenticating)"}, nil},
		{"first start: subscribing", monitor.Status{State: monitor.StateSubscribing}, []string{"Starting (subscribing)"}, nil},
		{"first start: synchronizing", monitor.Status{State: monitor.StateSynchronizing}, []string{"Starting (synchronizing)"}, nil},
		{"retry after network failures: connecting", monitor.Status{State: monitor.StateConnecting, LastFailure: monitor.FailureNetwork, Reconnects: 9, Tracked: 7},
			[]string{"Reconnecting to Home Assistant's WebSocket", "previous attempt failed: network problem", "Last known values may be outdated"}, []string{"Starting"}},
		{"retry after protocol failure: connecting", monitor.Status{State: monitor.StateConnecting, LastFailure: monitor.FailureProtocol, Reconnects: 2},
			[]string{"previous attempt failed: unexpected response"}, []string{"Starting"}},
		{"retry after token rejection: connecting", monitor.Status{State: monitor.StateConnecting, LastFailure: monitor.FailureAuthentication, Reconnects: 3},
			[]string{"rejected the access token", "HA_TOKEN", "retrying now"}, []string{"Starting"}},
		{"reconnection: authenticating", monitor.Status{State: monitor.StateAuthenticating, LastFailure: monitor.FailureNetwork, Reconnects: 1}, []string{"Reconnecting (authenticating)"}, []string{"Starting", "Cannot reach"}},
		{"reconnection: subscribing", monitor.Status{State: monitor.StateSubscribing, LastFailure: monitor.FailureNetwork, Reconnects: 1}, []string{"Reconnecting (subscribing)"}, []string{"Starting"}},
		{"reconnection: synchronizing", monitor.Status{State: monitor.StateSynchronizing, LastFailure: monitor.FailureNetwork, Reconnects: 1, Tracked: 4}, []string{"Reconnecting (synchronizing)", "may be outdated"}, []string{"Starting"}},
		{"waiting to retry: network", monitor.Status{State: monitor.StateReconnecting, LastFailure: monitor.FailureNetwork, NextRetryAt: retry, Tracked: 7, Reconnects: 4},
			[]string{"Cannot reach Home Assistant's WebSocket", "next attempt at <t:1793310000:T>", "Last known values may be outdated", "Reconnects: 4"}, []string{"Starting", ":R>"}},
		{"waiting to retry: token", monitor.Status{State: monitor.StateReconnecting, LastFailure: monitor.FailureAuthentication, NextRetryAt: retry},
			[]string{"rejected the access token", "next attempt at <t:1793310000:T>"}, []string{":R>"}},
		{"waiting to retry: protocol", monitor.Status{State: monitor.StateReconnecting, LastFailure: monitor.FailureProtocol, NextRetryAt: retry},
			[]string{"Unexpected response", "next attempt at"}, []string{":R>"}},
		{"healthy", monitor.Status{State: monitor.StateReady, Healthy: true, Transport: true, Authenticated: true, Subscribed: true, Synchronized: true, Tracked: 2, LastSyncAt: retry},
			[]string{"Healthy: 2 sensors tracked", "synchronized <t:1793310000:R>", "no events yet"}, []string{"Reconnecting", "Starting"}},
		{"ready but silent", monitor.Status{State: monitor.StateReady, Transport: true, Authenticated: true, Subscribed: true, Synchronized: true},
			[]string{"Connected but silent"}, []string{"Healthy"}},
		{"stopped", monitor.Status{State: monitor.StateStopped}, []string{"Stopped"}, []string{"Starting"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := commands.MonitoringText(&tt.st)

			mustContain(t, got, tt.want...)
			for _, bad := range tt.notWant {
				if strings.Contains(got, bad) {
					t.Errorf("must not contain %q:\n%s", bad, got)
				}
			}
		})
	}
}

// retryGate replaces the monitor's backoff sleep (Config.Sleep, the documented
// injection point) so a test, not a 2 ms timer, decides when the next dial may
// start. Every Sleep call announces itself on entered and then blocks until the
// test calls release or the monitor's context ends, so cleanup never hangs.
type retryGate struct {
	entered chan struct{}
	proceed chan struct{}
}

func newRetryGate() *retryGate {
	return &retryGate{entered: make(chan struct{}, 1), proceed: make(chan struct{})}
}

func (g *retryGate) sleep(ctx context.Context, _ time.Duration) error {
	select {
	case g.entered <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-g.proceed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// awaitSleep returns once the monitor is parked in its backoff, after it has
// published the failure and the retry time. Until release it cannot dial, so
// its status is stable.
func (g *retryGate) awaitSleep(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the monitor never scheduled a retry")
	}
}

func (g *retryGate) release(t *testing.T) {
	t.Helper()
	select {
	case g.proceed <- struct{}{}:
	case <-time.After(10 * time.Second):
		t.Fatal("no retry was waiting to be released")
	}
}

// The same states, reached by a real monitor instead of a hand-built status.
//
// Two synchronization points keep it deterministic (it used to depend on timing):
//   - the backoff is held (retryGate), so the monitor cannot move on to dialing
//     between the moment a state is observed and the moment /health is read;
//   - the held snapshot is released only after the fake server has received the
//     get_states request, because a request that arrives after ReleaseStates
//     would park on the next gate and never be answered.
func TestHealth_followsARealMonitorThroughOutageReconnectionAndRecovery(t *testing.T) {
	gate := newRetryGate()
	le := newLiveEnv(t, func(c *monitor.Config) { c.Sleep = gate.sleep })
	le.shareBoth(t)
	le.srv.Down()
	le.start()

	gate.awaitSleep(t) // the first dial failed; the monitor is waiting out its backoff
	le.wait(t, func(s monitor.Status) bool { return s.State == monitor.StateReconnecting && s.Reconnects >= 1 })
	outage := le.run(user, "health", "", nil)

	le.srv.Script(func(sc *fakeha.Script) { sc.HoldStates = true })
	le.srv.Up()
	gate.release(t) // now the retry may dial
	le.awaitHeldSnapshot(t)
	le.wait(t, func(s monitor.Status) bool { return s.State == monitor.StateSynchronizing && s.Reconnects >= 1 })
	reconnecting := le.run(user, "health", "", nil)

	le.srv.ReleaseStates() // safe: the server holds the request on the gate that this releases
	le.wait(t, func(s monitor.Status) bool { return s.Healthy })
	recovered := le.run(user, "health", "", nil)

	le.srv.Down()
	gate.awaitSleep(t) // the second outage is detected and the monitor is back in its backoff
	le.wait(t, func(s monitor.Status) bool { return s.State == monitor.StateReconnecting && !s.Healthy })
	secondOutage := le.run(user, "health", "", nil)

	mustContain(t, outage, "Cannot reach Home Assistant's WebSocket", "next attempt at <t:")
	mustContain(t, reconnecting, "Reconnecting (synchronizing)", "Subscribed \u2705", "Snapshot \u274C")
	mustContain(t, recovered, "Healthy: 2 sensors tracked", "Snapshot \u2705")
	mustContain(t, secondOutage, "Cannot reach Home Assistant's WebSocket", "Last known values may be outdated")
	for name, text := range map[string]string{"outage": outage, "reconnecting": reconnecting, "second outage": secondOutage} {
		if strings.Contains(text, "Starting") {
			t.Errorf("%s: a reconnection must never be called Starting:\n%s", name, text)
		}
		if strings.Contains(text, ":R>") {
			t.Errorf("%s: a retry must not use a relative timestamp that turns into \"ago\":\n%s", name, text)
		}
	}
	if strings.Contains(recovered, "Reconnecting") || strings.Contains(recovered, "Cannot reach") {
		t.Errorf("a recovered monitor must not keep reporting the old failure:\n%s", recovered)
	}
}

// awaitHeldSnapshot blocks until the fake server has received a get_states request
// that it is holding (HoldStates). The fake counts the request and captures its
// release gate under one lock, so after this returns ReleaseStates answers it.
func (le *liveEnv) awaitHeldSnapshot(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := le.srv.WaitWS(ctx, func(s fakeha.WSStats) bool { return s.StateRequests >= 1 }); err != nil {
		t.Fatalf("the server never received get_states: %v (%+v)", err, le.mon.Status())
	}
}

// The dial attempt itself (state "connecting") after earlier failures: the
// exact situation seen as "Starting (connecting)" with 9 reconnects.
func TestHealth_aRetryDialAfterFailuresIsReconnecting_notStarting(t *testing.T) {
	var dials atomic.Int32
	le := newLiveEnv(t, func(c *monitor.Config) {
		c.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() } // no real waiting
		c.Dial = func(ctx context.Context, _ ws.Options) (*ws.Session, error) {
			if dials.Add(1) <= 3 {
				return nil, fmt.Errorf("%w: connect failed", ws.ErrTransport)
			}
			<-ctx.Done() // the fourth attempt stays in progress: state "connecting"
			return nil, ctx.Err()
		}
	})
	le.start()

	st := le.wait(t, func(s monitor.Status) bool { return s.State == monitor.StateConnecting && s.Reconnects == 3 })
	got := le.run(user, "health", "", nil)

	if st.LastFailure != monitor.FailureNetwork {
		t.Fatalf("status = %+v", st)
	}
	mustContain(t, got, "Reconnecting to Home Assistant's WebSocket", "previous attempt failed: network problem", "Reconnects: 3")
	if strings.Contains(got, "Starting") {
		t.Errorf("reported Starting although it is retrying after 3 failures:\n%s", got)
	}
}
