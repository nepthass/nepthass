package notify

import (
	"sync"
	"time"

	"github.com/nepthass/nepthass/internal/monitor"
)

// The monitoring-down state machine decides when to tell the channel that
// Home Assistant monitoring has been unhealthy for too long, and when to say it
// is back. It is a pure function of observations and the clock, so it is tested
// with a fake clock and no goroutines.
//
//	Starting --healthy-------------------------------> Healthy
//	Starting --unhealthy (clock starts at process start)-> Down
//	Healthy/Recovered --unhealthy (clock starts now)---> Down
//	Down --healthy (cancel the timer, say nothing)-----> Healthy
//	Down --unhealthy for DownAfter (queue ONE alert)---> DownAlerted
//	DownAlerted --healthy, alert was DELIVERED---------> Recovered (queue a recovery message)
//	DownAlerted --healthy, alert not delivered---------> Recovered (withdraw the queued alert, say nothing)
//	any --intentional shutdown-------------------------> Stopped (never alerts)
//
// An alert is "delivered" only when the Sender reported success; being queued
// is not delivery. If the alert was dropped (TTL, overflow, broken channel) the
// machine re-sends it ONCE per outage, and only while delivery is possible again.
// Recovered behaves as Healthy: the next outage is a new episode.
type downState uint8

const (
	downStarting downState = iota + 1
	downHealthy
	downDown
	downAlerted
	downRecovered
	downStopped
)

func (s downState) String() string {
	switch s {
	case downStarting:
		return "starting"
	case downHealthy:
		return "healthy"
	case downDown:
		return "down"
	case downAlerted:
		return "down_alerted"
	case downRecovered:
		return "recovered"
	case downStopped:
		return "stopped"
	}
	return "unknown"
}

type downAction uint8

const (
	actNone downAction = iota
	actSendDown
	actSendRecovery
	actCancelDown
)

type downMachine struct {
	mu        sync.Mutex
	state     downState
	threshold time.Duration
	startedAt time.Time
	downSince time.Time
	episode   uint64

	delivered_, dropped_, resent, recoverySent bool
}

func newDownMachine(start time.Time, threshold time.Duration) *downMachine {
	return &downMachine{state: downStarting, threshold: threshold, startedAt: start}
}

// reset forgets any outage and starts over (used while notifications are disabled).
func (m *downMachine) reset(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != downStopped {
		m.state, m.startedAt = downStarting, now
	}
}

// step feeds one observation of monitoring health. deliveryAvailable says whether
// the transport is believed to be working (the circuit breaker is closed).
func (m *downMachine) step(now time.Time, healthy, deliveryAvailable bool) downAction {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.state {
	case downStopped:
		return actNone
	case downStarting:
		if healthy {
			m.state = downHealthy
			return actNone
		}
		m.beginDown(m.startedAt)
		return m.stepDown(now)
	case downHealthy, downRecovered:
		if healthy {
			return actNone
		}
		m.beginDown(now)
		return m.stepDown(now)
	case downDown:
		if healthy {
			m.state = downHealthy
			return actNone
		}
		return m.stepDown(now)
	case downAlerted:
		if healthy {
			m.state = downRecovered
			if m.delivered_ {
				m.recoverySent = true
				return actSendRecovery
			}
			return actCancelDown
		}
		if m.dropped_ && !m.resent && deliveryAvailable {
			m.resent, m.dropped_ = true, false
			return actSendDown
		}
	}
	return actNone
}

func (m *downMachine) beginDown(since time.Time) {
	m.state, m.downSince = downDown, since
	m.episode++
	m.delivered_, m.dropped_, m.resent, m.recoverySent = false, false, false, false
}

func (m *downMachine) stepDown(now time.Time) downAction {
	if now.Sub(m.downSince) >= m.threshold {
		m.state = downAlerted
		return actSendDown
	}
	return actNone
}

// episodeInfo returns the current outage episode and when it began.
func (m *downMachine) episodeInfo() (uint64, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.episode, m.downSince
}

// delivered records that the down alert of an episode reached the transport. It
// returns true when the outage had already ended while the alert was in flight
// (so it was withdrawn too late): the caller must then send the recovery message,
// or the channel would be left saying monitoring is down.
func (m *downMachine) delivered(episode uint64) (sendRecovery bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if episode != m.episode {
		return false
	}
	m.delivered_, m.dropped_ = true, false
	if m.state == downRecovered && !m.recoverySent {
		m.recoverySent = true
		return true
	}
	return false
}

// dropped records that the down alert of an episode was lost.
func (m *downMachine) dropped(episode uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if episode == m.episode && !m.delivered_ {
		m.dropped_ = true
	}
}

// shutdown makes the machine inert: an intentional shutdown is not an outage.
func (m *downMachine) shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = downStopped
}

func (m *downMachine) current() downState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// StatusSource reports monitoring health. *monitor.Monitor satisfies it.
type StatusSource interface {
	Status() monitor.Status
}
