package notify

import (
	"testing"
	"time"
)

func TestDownMachine_aFullOutageEpisode_oneAlert_recoveryOnlyAfterDelivery_thenANewEpisode(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	m := newDownMachine(t0, 5*time.Minute)

	if a := m.step(t0, true, true); a != actNone || m.current() != downHealthy {
		t.Fatalf("healthy at start: %v %v", a, m.current())
	}
	if a := m.step(at(time.Minute), false, true); a != actNone || m.current() != downDown {
		t.Fatalf("an unhealthy observation starts the timer: %v %v", a, m.current())
	}
	if a := m.step(at(time.Minute+5*time.Minute-time.Second), false, true); a != actNone {
		t.Fatalf("one second short of five minutes: %v", a)
	}
	if a := m.step(at(6*time.Minute), false, true); a != actSendDown || m.current() != downAlerted {
		t.Fatalf("five continuous minutes: %v %v", a, m.current())
	}
	for i := 7; i < 12; i++ {
		if a := m.step(at(time.Duration(i)*time.Minute), false, true); a != actNone {
			t.Fatalf("one alert per outage, never repeated while reconnecting: %v", a)
		}
	}
	if ep, since := m.episodeInfo(); ep != 1 || !since.Equal(at(time.Minute)) {
		t.Errorf("episode info = %d %v", ep, since)
	}
	if m.delivered(1) {
		t.Error("delivery during the outage needs no recovery yet")
	}
	if a := m.step(at(12*time.Minute), true, true); a != actSendRecovery || m.current() != downRecovered {
		t.Fatalf("recovery after a delivered alert: %v %v", a, m.current())
	}
	if a := m.step(at(13*time.Minute), true, true); a != actNone {
		t.Errorf("recovered stays quiet: %v", a)
	}
	m.step(at(14*time.Minute), false, true)
	if ep, _ := m.episodeInfo(); ep != 2 || m.current() != downDown {
		t.Errorf("the next outage is a new episode: %d %v", ep, m.current())
	}
}

func TestDownMachine_startingWithoutAHealthyConnectionStartsTheTimerAtProcessStart(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	m := newDownMachine(t0, 5*time.Minute)
	if a := m.step(t0.Add(time.Minute), false, true); a != actNone || m.current() != downDown {
		t.Fatalf("%v %v", a, m.current())
	}
	if a := m.step(t0.Add(5*time.Minute), false, true); a != actSendDown {
		t.Errorf("never healthy since start: the five minutes count from the start, got %v", a)
	}
}

func TestDownMachine_recoveryBeforeTheThresholdCancelsTheTimer_andFlappingNeverAlerts(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	m := newDownMachine(t0, 5*time.Minute)
	m.step(t0, true, true)
	now := t0
	for cycle := 0; cycle < 6; cycle++ {
		now = now.Add(time.Minute)
		m.step(now, false, true)
		now = now.Add(4 * time.Minute)
		if a := m.step(now, false, true); a != actNone {
			t.Fatalf("cycle %d: 4 minutes down is below the threshold: %v", cycle, a)
		}
		now = now.Add(time.Second)
		if a := m.step(now, true, true); a != actNone {
			t.Fatalf("cycle %d: recovering before the alert says nothing: %v", cycle, a)
		}
	}
	if ep, _ := m.episodeInfo(); ep != 6 {
		t.Errorf("each unhealthy period is its own episode: %d", ep)
	}
}

func TestDownMachine_queuedIsNotDelivered_recoveryWithdrawsAnUndeliveredAlertAndSaysNothing(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	m := newDownMachine(t0, 5*time.Minute)
	m.step(t0, false, true)
	if a := m.step(t0.Add(5*time.Minute), false, true); a != actSendDown {
		t.Fatal(a)
	}
	if a := m.step(t0.Add(6*time.Minute), true, true); a != actCancelDown {
		t.Errorf("the alert was only queued, so recovery withdraws it instead of announcing a recovery: %v", a)
	}
}

func TestDownMachine_aDroppedAlertIsResentOncePerOutage_onlyWhileDeliveryIsAvailable(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	m := newDownMachine(t0, 5*time.Minute)
	m.step(t0, false, true)
	m.step(t0.Add(5*time.Minute), false, true)
	m.dropped(1)
	if a := m.step(t0.Add(6*time.Minute), false, false); a != actNone {
		t.Errorf("no resend while the transport is failing: %v", a)
	}
	if a := m.step(t0.Add(7*time.Minute), false, true); a != actSendDown {
		t.Errorf("one resend once delivery is possible: %v", a)
	}
	m.dropped(1)
	if a := m.step(t0.Add(8*time.Minute), false, true); a != actNone {
		t.Errorf("never a second resend in the same outage: %v", a)
	}
}

func TestDownMachine_anAlertDeliveredAfterTheOutageEndedGetsItsRecovery(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	m := newDownMachine(t0, 5*time.Minute)
	m.step(t0, false, true)
	m.step(t0.Add(5*time.Minute), false, true)
	if a := m.step(t0.Add(6*time.Minute), true, true); a != actCancelDown {
		t.Fatal(a)
	}
	if !m.delivered(1) {
		t.Error("the alert was in flight and reached the channel after recovery: the caller must send the recovery message")
	}
	if m.delivered(1) {
		t.Error("only once")
	}
	m.delivered(0) // feedback for another episode is ignored
}

func TestDownMachine_shutdownAndDisabledNotificationsNeverAlert(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	m := newDownMachine(t0, 5*time.Minute)
	m.step(t0, false, true)
	m.shutdown()
	for i := 1; i <= 20; i++ {
		if a := m.step(t0.Add(time.Duration(i)*time.Minute), false, true); a != actNone {
			t.Fatalf("an intentional shutdown is not an outage: %v", a)
		}
	}

	d := newDownMachine(t0, 5*time.Minute)
	d.step(t0, false, true)
	d.reset(t0.Add(4 * time.Minute)) // notifications were disabled: the episode is forgotten
	if a := d.step(t0.Add(6*time.Minute), false, true); a != actNone {
		t.Errorf("the clock restarts at the reset: %v", a)
	}
}
