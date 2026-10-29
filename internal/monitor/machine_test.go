package monitor

import (
	"testing"
	"time"
)

func TestStateMachine_allowsOnlyTheDocumentedTransitions(t *testing.T) {
	all := []ConnState{StateDisconnected, StateConnecting, StateAuthenticating, StateSubscribing, StateSynchronizing, StateReady, StateReconnecting, StateStopped}
	allowed := map[ConnState]map[ConnState]bool{
		StateDisconnected:   {StateConnecting: true, StateStopped: true},
		StateConnecting:     {StateAuthenticating: true, StateReconnecting: true, StateStopped: true},
		StateAuthenticating: {StateSubscribing: true, StateReconnecting: true, StateStopped: true},
		StateSubscribing:    {StateSynchronizing: true, StateReconnecting: true, StateStopped: true},
		StateSynchronizing:  {StateReady: true, StateReconnecting: true, StateStopped: true},
		StateReady:          {StateReconnecting: true, StateStopped: true},
		StateReconnecting:   {StateConnecting: true, StateStopped: true},
		StateStopped:        {},
	}
	for _, from := range all {
		for _, to := range all {
			if got, want := from.CanTransitionTo(to), allowed[from][to]; got != want {
				t.Errorf("%s -> %s: allowed=%v, want %v", from, to, got, want)
			}
		}
	}
}

func TestMachine_refusesIllegalTransitions_countsThem_andStaysPut(t *testing.T) {
	m := newMachine(base)

	_, okSkip := m.transition(StateReady, base) // cannot skip the handshake
	_, okStart := m.transition(StateConnecting, base)
	_, okBack := m.transition(StateDisconnected, base)

	if okSkip || !okStart || okBack {
		t.Errorf("skip=%v start=%v back=%v", okSkip, okStart, okBack)
	}
	if v := m.view(); v.state != StateConnecting || m.violations != 2 {
		t.Errorf("state=%s violations=%d", v.state, m.violations)
	}
}

func TestMachine_broadcastsEveryChange_toWaiters(t *testing.T) {
	m := newMachine(base)
	m.mu.Lock()
	ch := m.changed
	m.mu.Unlock()

	m.transition(StateConnecting, base)

	select {
	case <-ch:
	default:
		t.Error("a transition must wake waiters")
	}
}

func TestLayers_areDerivedFromTheState_soTheyCannotDisagree(t *testing.T) {
	tests := []struct {
		state                      ConnState
		transport, auth, sub, sync bool
	}{
		{StateDisconnected, false, false, false, false},
		{StateConnecting, false, false, false, false},
		{StateAuthenticating, true, false, false, false},
		{StateSubscribing, true, true, false, false},
		{StateSynchronizing, true, true, true, false},
		{StateReady, true, true, true, true},
		{StateReconnecting, false, false, false, false},
		{StateStopped, false, false, false, false},
	}
	for _, tt := range tests {
		tr, a, s, y := layersFor(tt.state)
		if tr != tt.transport || a != tt.auth || s != tt.sub || y != tt.sync {
			t.Errorf("%s: %v %v %v %v", tt.state, tr, a, s, y)
		}
	}
}

func TestBackoff_growsExponentially_toTheCap_withBoundedJitter(t *testing.T) {
	full := func(n int64) int64 { return n - 1 } // jitter at its maximum: the full delay
	none := func(int64) int64 { return 0 }       // jitter at its minimum: half the delay
	b := Backoff{Initial: time.Second, Max: 8 * time.Second, AuthInitial: 30 * time.Second, AuthMax: 5 * time.Minute, Rand: full}

	var got []time.Duration
	for range 6 {
		got = append(got, b.Next(FailureNetwork))
	}
	b.Reset()
	b.Rand = none
	low := b.Next(FailureNetwork)

	want := []time.Duration{1, 2, 4, 8, 8, 8}
	for i, w := range want {
		if got[i] != w*time.Second {
			t.Errorf("delay %d = %v, want %v", i, got[i], w*time.Second)
		}
	}
	if low != 500*time.Millisecond {
		t.Errorf("minimum jitter = %v, want half of the first delay", low)
	}
}

func TestBackoff_authenticationFailuresUseAnIndependentSlowSchedule(t *testing.T) {
	b := Backoff{Initial: time.Second, Max: time.Minute, AuthInitial: 30 * time.Second, AuthMax: 10 * time.Minute, Rand: func(n int64) int64 { return n - 1 }}

	first := b.Next(FailureAuthentication)
	second := b.Next(FailureAuthentication)
	network := b.Next(FailureNetwork) // does not inherit the auth schedule
	for range 10 {
		b.Next(FailureAuthentication)
	}
	capped := b.Next(FailureAuthentication)

	if first != 30*time.Second || second != time.Minute || network != time.Second || capped != 10*time.Minute {
		t.Errorf("first=%v second=%v network=%v capped=%v", first, second, network, capped)
	}
}

func TestBackoff_neverOverflows_andJitterStaysInsideTheRange(t *testing.T) {
	b := Backoff{Initial: time.Second, Max: time.Minute, Rand: func(n int64) int64 { return n / 2 }}

	for i := range 200 {
		scheduled := grow(time.Second, time.Minute, i)
		d := b.Next(FailureProtocol)
		if d < scheduled/2 || d > scheduled {
			t.Fatalf("attempt %d: delay %v outside [%v, %v]", i, d, scheduled/2, scheduled)
		}
	}
	if b.Attempts() != 200 {
		t.Errorf("attempts = %d", b.Attempts())
	}
}
