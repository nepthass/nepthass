package notify

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
)

// Initial-state alerting (decision D5).
//
// NeptHass alerts on events. When it starts it has states, not events, and a state
// does not prove when it began or even that it is current (a sleeping device keeps
// reporting its last known state). So a sensor found ALREADY violating a rule at
// its first reliable observation is handled as a baseline, not as a transition:
//
//   - a hazard (the water leak) is announced once, at once, on the critical lane,
//     worded as what Home Assistant reports and not as something that just happened;
//   - any other rule is listed in ONE summary sent after a short window, so a start
//     with many open doors is a single message, never a storm;
//   - a duration rule starts counting from the first sighting (see startEpisode);
//   - nothing is sent for an unavailable, unknown or invalid reading, for a rule
//     that is paused or whose sensor is not shared, or while notifications are off.
//
// A reconnect inside one process is NOT a start: the tracker still knows the
// previous state, so v0.4.0 behavior (late alerts) applies unchanged.
//
// Persistence is minimal and written only AFTER a confirmed delivery. A delivered
// hazard alert leaves a mark (rule, episode identity, time) so a restart does not
// announce the same episode again; the time of the last summary is kept in the
// settings so restarts cannot flood the channel. The delivery acknowledgement and
// the SQLite write are not atomic. If the process dies after Discord accepted a
// message and before the mark is written, the alert is announced again once after
// the restart: a duplicate, never a loss. If it dies before delivery, nothing was
// recorded and the alert is announced after the restart.

// startupState is the evaluator's memory of this start. Evaluator goroutine only.
type startupState struct {
	firstSight   time.Time // when the first baseline condition was evaluated
	flushed      bool      // the summary window has closed
	hazardAlerts int       // individual hazard alerts raised so far
	listed       map[int64]struct{}
	lines        []DigestLine // non-hazard conditions waiting for the end of the window
	overflow     []DigestLine // hazards beyond the budget, sent at the end of this evaluation
}

// episodeKey identifies "the same episode" of a violating sensor across restarts:
// Home Assistant's last_changed of the violating state. It is an identity only,
// never a claim about when anything began.
func episodeKey(changed time.Time) string { return changed.UTC().Format(time.RFC3339Nano) }

// initialState handles a rule whose sensor NeptHass first sees already violating.
func (e *Engine) initialState(v *slotEnv, r cachedRule, st *ruleState, s monitor.Sensor, at time.Time) {
	if e.startup.firstSight.IsZero() {
		e.startup.firstSight = v.now
	}
	switch {
	case r.Duration > 0:
		e.startEpisode(st, at, r.Duration, false) // counted from the first sighting, never earlier
	case s.Category.Hazard():
		e.initialHazard(v, r, st, s)
	default:
		if r.HasClear {
			st.armed = false // it must clear before it can fire
		}
		e.collectStartup(v, r, s)
	}
}

func (e *Engine) digestLine(r cachedRule, id entities.ID, s monitor.Sensor, aliases map[entities.ID]string) DigestLine {
	in := s.Interpretation()
	line := DigestLine{Category: s.Category, Name: displayName(id, s, aliases), Trigger: r.Kind, Meaning: in.Meaning, Value: in.Value,
		HasValue: in.HasValue, Unit: s.Unit, ChangedAt: s.LastChanged, rule: r.ID, entity: id}
	if r.Kind != KindState {
		t := r.Threshold
		line.Threshold = &t
	}
	return line
}

// initialHazard announces a hazard that is already present. It is suppressed only
// when the same episode was already delivered (or an alert for this rule was
// delivered within its cooldown), and it is bounded by a per-start budget: hazards
// beyond it are folded into one critical message instead of one message each.
func (e *Engine) initialHazard(v *slotEnv, r cachedRule, st *ruleState, s monitor.Sensor) {
	if !v.permitted(r.ID) {
		return
	}
	if e.alreadyAnnounced(v.ctx, r, s, v.now) {
		e.m.markSuppressed.Add(1)
		return
	}
	if e.startup.hazardAlerts >= e.cfg.StartupHazardBudget {
		e.startup.overflow = append(e.startup.overflow, e.digestLine(r, v.id, s, v.names()))
		return
	}
	e.startup.hazardAlerts++
	e.m.initialAlerts.Add(1)
	e.release(v, r, st, s.Category, func(muted int) Message {
		in := s.Interpretation()
		return Message{Kind: MsgAlert, RuleID: r.ID, Category: s.Category, Name: displayName(v.id, s, v.names()), Trigger: r.Kind,
			Meaning: in.Meaning, Unit: s.Unit, ObservedAt: v.now, ChangedAt: s.LastChanged, ReportedAt: s.LastUpdated,
			Initial: true, Muted: muted, entity: v.id}
	})
}

// alreadyAnnounced reports whether a delivered hazard alert covers this episode: the
// same Home Assistant episode, or any delivery within the rule's cooldown. A mark
// that cannot be read never suppresses a hazard: the cost is at most one repeat.
func (e *Engine) alreadyAnnounced(ctx context.Context, r cachedRule, s monitor.Sensor, now time.Time) bool {
	m, ok, err := e.d.Store.GetMark(ctx, r.ID)
	if err != nil {
		e.log.Warn("could not read a hazard delivery mark; the alert is not suppressed", "error", err)
		return false
	}
	if !ok {
		return false
	}
	return m.EpisodeKey == episodeKey(s.LastChanged) || now.Sub(m.DeliveredAt) < cooldownOf(r, s.Category)
}

// collectStartup adds a non-hazard condition to the start-up summary while the
// window is open. After the window a newly baselined condition is silent.
func (e *Engine) collectStartup(v *slotEnv, r cachedRule, s monitor.Sensor) {
	su := &e.startup
	if v.reload || su.flushed || !v.now.Before(su.firstSight.Add(e.cfg.StartupWindow)) {
		return
	}
	if _, dup := su.listed[r.ID]; dup || !v.permitted(r.ID) {
		return
	}
	if su.listed == nil {
		su.listed = map[int64]struct{}{}
	}
	su.listed[r.ID] = struct{}{}
	su.lines = append(su.lines, e.digestLine(r, v.id, s, v.names()))
	e.m.startupListed.Add(1)
}

// flushStartup sends hazards beyond the budget at once and, when the window has
// closed, the one summary. The summary is skipped when one was delivered within
// SummaryMinInterval, a time that survives restarts.
func (e *Engine) flushStartup(ctx context.Context) {
	su := &e.startup
	if len(su.overflow) > 0 {
		lines := su.overflow
		su.overflow = nil
		e.enqueueStartup(lines)
	}
	now := e.clock.Now()
	if su.flushed || su.firstSight.IsZero() || now.Before(su.firstSight.Add(e.cfg.StartupWindow)) {
		return
	}
	su.flushed = true
	lines := su.lines
	su.lines = nil
	if len(lines) == 0 {
		return
	}
	if st, ok, err := e.d.Store.GetSettings(ctx, e.d.GuildID); err == nil && ok && !st.StartupSummaryAt.IsZero() && now.Sub(st.StartupSummaryAt) < e.cfg.SummaryMinInterval {
		e.m.summarySuppressed.Add(1)
		return
	}
	e.enqueueStartup(lines)
}

func (e *Engine) enqueueStartup(lines []DigestLine) {
	slices.SortFunc(lines, func(a, b DigestLine) int { return cmp.Compare(a.rule, b.rule) })
	more := 0
	if len(lines) > e.cfg.DigestMaxLines {
		more = len(lines) - e.cfg.DigestMaxLines
		lines = lines[:e.cfg.DigestMaxLines]
	}
	e.m.startupSummaries.Add(1)
	e.enqueue(Message{Kind: MsgStartup, Lines: lines, MoreLines: more, ObservedAt: e.clock.Now()})
}

// startupDelivered records what a delivered start-up message makes durable: the
// summary time (so a restart does not repeat it inside the window) and a mark for
// every hazard it carried.
func (e *Engine) startupDelivered(m Message) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, l := range m.Lines {
		if l.Category.Hazard() {
			e.putMarkCtx(ctx, l.rule, l.ChangedAt)
		}
	}
	for _, l := range m.Lines {
		if !l.Category.Hazard() {
			if _, err := e.d.Store.SetStartupSummary(ctx, e.d.GuildID, e.clock.Now()); err != nil {
				e.log.Warn("could not record the time of the start-up summary; a restart inside the window may repeat it", "error", err)
			}
			return
		}
	}
}

// putMark records the delivery of a hazard alert. A failure is logged and never
// blocks delivery: the worst case is one repeat after a restart.
func (e *Engine) putMark(rule int64, changed time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	e.putMarkCtx(ctx, rule, changed)
}

func (e *Engine) putMarkCtx(ctx context.Context, rule int64, changed time.Time) {
	if err := e.d.Store.PutMark(ctx, Mark{RuleID: rule, EpisodeKey: episodeKey(changed), DeliveredAt: e.clock.Now()}); err != nil {
		e.log.Warn("could not record the delivery of a hazard alert; a restart may announce it once more", "error", err)
	}
}
