package commands_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/config"
	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/homeassistant/ws"
	"github.com/nepthass/nepthass/internal/monitor"
	"github.com/nepthass/nepthass/tests/fakeha"
)

const liveToken = "live-token"

// liveEnv wires the real Reader and a real Monitor to its own fake Home
// Assistant, so tests can drive the monitor through its real states.
type liveEnv struct {
	env
	mon     *monitor.Monitor
	srv     *fakeha.Server
	cancel  context.CancelFunc
	started bool
}

func newLiveEnv(t *testing.T, mod func(*monitor.Config)) *liveEnv {
	t.Helper()
	srv := fakeha.New(t, liveToken)
	srv.Set("sensor.temp", "21.5", map[string]any{"device_class": "temperature", "unit_of_measurement": "\u00b0C", "friendly_name": "Temp"})
	srv.Set("binary_sensor.door", "off", map[string]any{"device_class": "door", "friendly_name": "Door"})
	cfg := monitor.Config{
		HomeAssistant: config.HomeAssistant{URL: srv.URL, Token: liveToken},
		Backoff: monitor.Backoff{
			Initial: 2 * time.Millisecond, Max: 16 * time.Millisecond, AuthInitial: 20 * time.Millisecond, AuthMax: 80 * time.Millisecond,
			StableAfter: time.Hour, Rand: func(n int64) int64 { return n - 1 },
		},
		Session: ws.Options{HandshakeTimeout: time.Second, PingInterval: time.Hour},
	}
	if mod != nil {
		mod(&cfg)
	}
	m, err := monitor.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	le := &liveEnv{mon: m, srv: srv}
	le.env = newEnvWith(t, func(p *entities.Permissions, a *entities.Aliases) commands.Monitoring {
		return monitor.NewReader(m, p, a)
	})
	t.Cleanup(func() {
		srv.ReleaseStates()
		if le.started {
			le.cancel()
			select {
			case <-m.Done():
			case <-time.After(10 * time.Second):
				t.Error("the monitor did not stop")
			}
		}
	})
	return le
}

func (le *liveEnv) start() {
	ctx, cancel := context.WithCancel(context.Background())
	le.cancel, le.started = cancel, true
	go le.mon.Run(ctx)
}

func (le *liveEnv) wait(t *testing.T, pred func(monitor.Status) bool) monitor.Status {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := le.mon.WaitFor(ctx, pred); err != nil {
		t.Fatalf("monitoring condition not reached: %v (%+v)", err, le.mon.Status())
	}
	return le.mon.Status()
}

func (le *liveEnv) shareBoth(t *testing.T) {
	t.Helper()
	le.grant(t, "sensor.temp", entities.AccessRead)
	le.grant(t, "binary_sensor.door", entities.AccessRead)
}

const nothingShared = "No monitored sensors are shared with you"

func TestSensors_beforeTheFirstSynchronization_neverClaimNothingIsShared(t *testing.T) {
	for _, tt := range []struct {
		name   string
		setup  func(t *testing.T) *liveEnv
		expect []string
	}{
		{"monitor not running yet", func(t *testing.T) *liveEnv { return newLiveEnv(t, nil) }, []string{"not running", "not available yet"}},
		{"subscribed but the snapshot is still being fetched", func(t *testing.T) *liveEnv {
			le := newLiveEnv(t, nil)
			le.srv.Script(func(sc *fakeha.Script) { sc.HoldStates = true })
			le.start()
			if err := le.srv.WaitWS(context.Background(), func(s fakeha.WSStats) bool { return s.StateRequests == 1 }); err != nil {
				t.Fatal(err)
			}
			le.wait(t, func(s monitor.Status) bool { return s.State == monitor.StateSynchronizing })
			return le
		}, []string{"still starting", "not available yet"}},
		{"Home Assistant is offline at startup", func(t *testing.T) *liveEnv {
			le := newLiveEnv(t, nil)
			le.srv.Down()
			le.start()
			le.wait(t, func(s monitor.Status) bool {
				return s.State == monitor.StateReconnecting && s.LastFailure == monitor.FailureNetwork
			})
			return le
		}, []string{"has not reached Home Assistant", "keeps retrying", "not available yet"}},
		{"the access token is rejected at startup", func(t *testing.T) *liveEnv {
			le := newLiveEnv(t, func(c *monitor.Config) { c.HomeAssistant.Token = "wrong" })
			le.start()
			le.wait(t, func(s monitor.Status) bool { return s.LastFailure == monitor.FailureAuthentication })
			return le
		}, []string{"cannot sign in", "HA_TOKEN"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			le := tt.setup(t)
			le.shareBoth(t) // permissions already exist: that must not be contradicted

			got := le.run(user, "sensors", "", nil)

			mustContain(t, got, tt.expect...)
			if strings.Contains(got, nothingShared) {
				t.Errorf("claims nothing is shared although sensors are shared and monitoring simply has not synchronized:\n%s", got)
			}
		})
	}
}

func TestSensors_beforeTheFirstSynchronization_revealsNothingAboutPermissions(t *testing.T) {
	shared, unshared := newLiveEnv(t, nil), newLiveEnv(t, nil)
	shared.shareBoth(t)

	withShares := shared.run(user, "sensors", "", nil)
	withoutShares := unshared.run(user, "sensors", "", nil)
	stranger := shared.run(auth.Actor{UserID: "9", Level: auth.LevelNone}, "sensors", "", nil)

	if withShares != withoutShares {
		t.Errorf("the pre-synchronization answer must be identical whether or not anything is shared:\n%q\n%q", withShares, withoutShares)
	}
	mustContain(t, stranger, "not authorized")
}

func TestSensors_afterSynchronization_stillSaysNothingIsSharedWhenThatIsTrue(t *testing.T) {
	le := newLiveEnv(t, nil)
	le.start()
	le.wait(t, func(s monitor.Status) bool { return s.Healthy })

	none := le.run(user, "sensors", "", nil)
	le.shareBoth(t)
	some := le.run(user, "sensors", "", nil)

	mustContain(t, none, nothingShared)
	if strings.Contains(none, "not available yet") {
		t.Errorf("a synchronized monitor with no shares must use the real empty state:\n%s", none)
	}
	mustContain(t, some, "Temp", "Door", "21.5")
}

func TestSensors_afterAnOutageThatFollowedASynchronization_keepsShowingLastKnownValues(t *testing.T) {
	le := newLiveEnv(t, nil)
	le.shareBoth(t)
	le.start()
	le.wait(t, func(s monitor.Status) bool { return s.Healthy })

	le.srv.Down()
	le.wait(t, func(s monitor.Status) bool { return s.State == monitor.StateReconnecting })
	got := le.run(user, "sensors", "", nil)

	mustContain(t, got, "Temp", "21.5", "last known values")
	if strings.Contains(got, nothingShared) || strings.Contains(got, "not available yet") {
		t.Errorf("after a successful synchronization the cache is real data, even while stale:\n%s", got)
	}
}
