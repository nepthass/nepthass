package monitor

import (
	"testing"
	"time"
)

// placeholder is what a sensor update without any device_class looks like.
var placeholder = map[string]any{"restored": true}

func doorTracker(t *testing.T) *Tracker {
	t.Helper()
	tr := NewTracker()
	u, err := ParseState(stateJSON("binary_sensor.door", "off", map[string]any{"device_class": "door", "friendly_name": "Front Door"}, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	tr.Apply(u, base)
	return tr
}

func applyEvent(t *testing.T, tr *Tracker, id, state string, attrs map[string]any, ms int) Result {
	t.Helper()
	u, err := ParseEvent(eventJSON(id, nil, stateJSON(id, state, attrs, ms, ms)))
	if err != nil {
		t.Fatal(err)
	}
	return tr.Apply(u, base.Add(time.Duration(ms)*time.Millisecond))
}

func TestTracker_aSensorThatBecomesUnavailableWithoutADeviceClassKeepsItsClass_andIsNeverShownAsSafe(t *testing.T) {
	for _, state := range []string{"unavailable", "unknown"} {
		t.Run(state, func(t *testing.T) {
			tr := doorTracker(t)

			res := applyEvent(t, tr, "binary_sensor.door", state, placeholder, 5)
			got, tracked := tr.Get("binary_sensor.door")

			if !tracked || res.Outcome != OutcomeApplied || !res.ValueChanged {
				t.Fatalf("the sensor must stay tracked: tracked=%v result=%+v", tracked, res)
			}
			if got.Category != CategoryDoor || got.DeviceClass != "door" || got.Raw != state || got.Name != "Front Door" || got.Revision != 2 {
				t.Errorf("class, device class and name must be retained: %+v", got)
			}
			if in := got.Interpretation(); in.Meaning.Reliable() || in.Meaning == MeaningClosed || in.Meaning == MeaningOpen {
				t.Errorf("a not-reporting sensor must never read as open, closed or any real state: %+v", in)
			}
		})
	}
}

func TestTracker_aRetainedSensorRecoversNormallyWhenItReportsAgain(t *testing.T) {
	tr := doorTracker(t)
	applyEvent(t, tr, "binary_sensor.door", "unavailable", placeholder, 5)

	res := applyEvent(t, tr, "binary_sensor.door", "on", map[string]any{"device_class": "door", "friendly_name": "Front Door"}, 9)
	got, _ := tr.Get("binary_sensor.door")

	if res.Outcome != OutcomeApplied || got.Interpretation().Meaning != MeaningOpen || got.Revision != 3 {
		t.Errorf("result=%+v sensor=%+v", res, got)
	}
}

func TestTracker_aRealValueWithoutADeviceClass_stopsTrackingBecauseItCannotBeInterpreted(t *testing.T) {
	for _, state := range []string{"on", "off", "21.5", "banana", ""} {
		t.Run("state="+state, func(t *testing.T) {
			tr := doorTracker(t)

			res := applyEvent(t, tr, "binary_sensor.door", state, placeholder, 5)

			if _, tracked := tr.Get("binary_sensor.door"); tracked || res.Outcome != OutcomeRemoved {
				t.Errorf("a real value with no class must not be interpreted with the old class: tracked=%v result=%+v", tracked, res)
			}
		})
	}
}

func TestTracker_anExplicitDifferentClassIsNeverOverriddenByTheRetainedOne(t *testing.T) {
	t.Run("a class we do not monitor", func(t *testing.T) {
		tr := doorTracker(t)

		res := applyEvent(t, tr, "binary_sensor.door", "unavailable", map[string]any{"device_class": "garage_door"}, 5)

		if _, tracked := tr.Get("binary_sensor.door"); tracked || res.Outcome != OutcomeRemoved {
			t.Errorf("tracked=%v result=%+v", tracked, res)
		}
	})
	t.Run("another monitored class", func(t *testing.T) {
		tr := doorTracker(t)

		applyEvent(t, tr, "binary_sensor.door", "unavailable", map[string]any{"device_class": "window"}, 5)
		got, tracked := tr.Get("binary_sensor.door")

		if !tracked || got.Category != CategoryWindow || got.DeviceClass != "window" {
			t.Errorf("an explicit class must win: tracked=%v %+v", tracked, got)
		}
	})
}

func TestTracker_aSensorNeverSeenWithAClassIsNotTrackedWhenItShowsUpUnavailableWithoutOne(t *testing.T) {
	tr := NewTracker()

	res := applyEvent(t, tr, "binary_sensor.mystery", "unavailable", placeholder, 5)

	if tr.Len() != 0 || res.Outcome != OutcomeUntracked {
		t.Errorf("there is no class to retain, so nothing may be invented: len=%d result=%+v", tr.Len(), res)
	}
}

func TestTracker_retentionStillObeysTheOrderingRules(t *testing.T) {
	tr := doorTracker(t)
	applyEvent(t, tr, "binary_sensor.door", "off", map[string]any{"device_class": "door"}, 50)

	stale := applyEvent(t, tr, "binary_sensor.door", "unavailable", placeholder, 10) // older than the stored state
	got, _ := tr.Get("binary_sensor.door")

	if stale.Outcome != OutcomeStale || got.Raw != "off" {
		t.Errorf("an old not-reporting update must not overwrite a newer value: %+v %+v", stale, got)
	}
}

func TestReconcile_retainsASensorThatTheSnapshotShowsUnavailableWithoutAClass(t *testing.T) {
	snapshot := func(state string, attrs map[string]any) []Update {
		doorU, err := ParseState(stateJSON("binary_sensor.door", state, attrs, 20, 20))
		if err != nil {
			t.Fatal(err)
		}
		strangerU, err := ParseState(stateJSON("binary_sensor.stranger", "unavailable", placeholder, 20, 20))
		if err != nil {
			t.Fatal(err)
		}
		return []Update{doorU, strangerU}
	}

	t.Run("unavailable without a class is retained", func(t *testing.T) {
		tr := doorTracker(t)

		r := tr.Reconcile(snapshot("unavailable", placeholder), base)
		got, tracked := tr.Get("binary_sensor.door")

		if !tracked || got.Category != CategoryDoor || got.Raw != "unavailable" || r.Updated != 1 || r.Removed != 0 {
			t.Errorf("reconciled=%+v tracked=%v sensor=%+v", r, tracked, got)
		}
		if _, invented := tr.Get("binary_sensor.stranger"); invented || tr.Len() != 1 {
			t.Errorf("a sensor that was never classified must not be invented: len=%d", tr.Len())
		}
	})
	t.Run("a real value without a class is dropped", func(t *testing.T) {
		tr := doorTracker(t)

		r := tr.Reconcile(snapshot("on", placeholder), base)

		if _, tracked := tr.Get("binary_sensor.door"); tracked || r.Removed != 1 {
			t.Errorf("reconciled=%+v tracked=%v", r, tracked)
		}
	})
}

func TestMonitor_aTrackedSensorThatTurnsUnavailableWithoutAClassStaysVisibleAsNotReporting(t *testing.T) {
	r := newRig(t, nil).start()
	r.waitReady()

	r.srv.Update("binary_sensor.door", "unavailable", placeholder) // an update with no device_class at all
	r.waitSensor("binary_sensor.door", "unavailable")
	got, tracked := r.m.Tracker().Get("binary_sensor.door")
	r.srv.Update("binary_sensor.door", "on", doorAttrs)
	r.waitSensor("binary_sensor.door", "on")
	back, _ := r.m.Tracker().Get("binary_sensor.door")

	if !tracked || got.Category != CategoryDoor || got.Interpretation().Meaning != MeaningUnavailable {
		t.Errorf("through the real WebSocket path: tracked=%v %+v", tracked, got)
	}
	if back.Interpretation().Meaning != MeaningOpen {
		t.Errorf("after recovery: %+v", back)
	}
}
