package monitor

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// syncSink receives events from the session's reader goroutine. While the
// snapshot is being fetched it buffers them (bounded); once the snapshot has
// been applied it replays the buffer through the normal ordering rule and
// switches to applying events directly. The single reader goroutine plus the
// lock around the switch guarantee no event is lost or reordered.
type syncSink struct {
	m        *Monitor
	mu       sync.Mutex
	live     bool
	buf      []json.RawMessage
	max      int
	overflow bool
}

func newSyncSink(m *Monitor, max int) *syncSink { return &syncSink{m: m, max: max} }

// handle is the ws.Handler. json.RawMessage values passed to it are copies
// owned by the frame, so buffering them is safe.
func (s *syncSink) handle(_ int64, event json.RawMessage) {
	s.mu.Lock()
	if !s.live {
		if len(s.buf) >= s.max {
			s.overflow = true
		} else {
			s.buf = append(s.buf, event)
		}
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.m.process(event)
}

// synchronize applies the authoritative snapshot, then replays the events that
// arrived while it was being fetched, then goes live. Buffered events older
// than the snapshot are dropped by the ordering rule; newer ones are applied.
func (m *Monitor) synchronize(sink *syncSink, raw json.RawMessage) (Reconciled, error) {
	snapshot, err := m.parseSnapshot(raw)
	if err != nil {
		return Reconciled{}, err
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.overflow {
		m.dropped.Add(1)
		return Reconciled{}, fmt.Errorf("%w (more than %d events)", errSyncOverflow, sink.max)
	}
	summary := m.tracker.Reconcile(snapshot, m.now())
	for _, ev := range sink.buf {
		m.process(ev)
	}
	sink.buf, sink.live = nil, true
	return summary, nil
}

// parseSnapshot validates a get_states result. Entities that are invalid are
// counted and skipped; entities outside the monitored domains are ignored.
func (m *Monitor) parseSnapshot(raw json.RawMessage) ([]Update, error) {
	var states []json.RawMessage
	if err := json.Unmarshal(raw, &states); err != nil {
		return nil, fmt.Errorf("%w: the state snapshot is not a list", ErrInvalid)
	}
	out := make([]Update, 0, len(states)/4)
	for _, s := range states {
		u, err := ParseState(s)
		switch {
		case err == nil:
			out = append(out, u)
		case errors.Is(err, errIgnored):
		default:
			m.invalid.Add(1)
		}
	}
	return out, nil
}
