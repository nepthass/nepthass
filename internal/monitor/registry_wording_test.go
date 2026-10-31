package monitor

import "testing"

// A row must carry its own wording, so a new class cannot inherit another class's alert text.
func TestRegistry_everyBinaryClassDeclaresItsOwnWording(t *testing.T) {
	for _, s := range Specs() {
		if s.Value == ValueBinary && (s.Alert == "" || s.Held == "" || s.Phrase == "") {
			t.Errorf("%s: a binary class declares Alert, Held and Phrase", s.Category)
		}
		if s.Value == ValueNumeric && (s.Alert != "" || s.Held != "") {
			t.Errorf("%s: a numeric class is worded by its threshold, not by a state phrase", s.Category)
		}
	}
}
