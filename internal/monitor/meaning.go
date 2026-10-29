package monitor

import (
	"math"
	"strconv"
	"strings"
)

// Meaning is what a raw state means for its category. The raw state is always
// kept next to it; the meaning is derived, never stored in place of it.
type Meaning string

const (
	MeaningDetected    Meaning = "detected"
	MeaningClear       Meaning = "clear"
	MeaningOpen        Meaning = "open"
	MeaningClosed      Meaning = "closed"
	MeaningWet         Meaning = "wet"
	MeaningDry         Meaning = "dry"
	MeaningLow         Meaning = "low"
	MeaningNormal      Meaning = "normal"
	MeaningReading     Meaning = "reading"
	MeaningInvalid     Meaning = "invalid"
	MeaningUnavailable Meaning = "unavailable"
	MeaningUnknown     Meaning = "unknown"
)

// Text is a plain-language rendering of a meaning for a category.
func (m Meaning) Text(c Category) string {
	switch m {
	case MeaningDetected:
		return "motion detected"
	case MeaningWet:
		return "water detected"
	case MeaningDry:
		return "dry"
	case MeaningLow:
		return "low"
	case MeaningNormal:
		return "normal"
	case MeaningInvalid:
		return "invalid value"
	case MeaningUnavailable:
		return "unavailable (not reporting)"
	case MeaningUnknown:
		return "unknown (not reporting)"
	case MeaningClear, MeaningOpen, MeaningClosed, MeaningReading:
	}
	return string(m)
}

// Reliable reports whether the meaning reflects a real reading. Unavailable,
// unknown and invalid are never to be treated as a safe or inactive state.
func (m Meaning) Reliable() bool {
	return m != MeaningUnavailable && m != MeaningUnknown && m != MeaningInvalid
}

// Interpretation is a meaning plus, for numeric categories, the parsed value.
type Interpretation struct {
	Meaning  Meaning
	Value    float64
	HasValue bool
}

// Interpret derives the meaning of a raw state. For binary sensors "on" means
// the condition is active: motion detected, door or window open, water
// detected, battery low. Anything outside the expected values is Invalid.
func Interpret(c Category, raw string) Interpretation {
	switch raw {
	case "unavailable":
		return Interpretation{Meaning: MeaningUnavailable}
	case "unknown":
		return Interpretation{Meaning: MeaningUnknown}
	}
	if c.Numeric() {
		return interpretNumber(c, raw)
	}
	on, off := binaryMeanings(c)
	switch raw {
	case "on":
		return Interpretation{Meaning: on}
	case "off":
		return Interpretation{Meaning: off}
	}
	return Interpretation{Meaning: MeaningInvalid}
}

func binaryMeanings(c Category) (on, off Meaning) {
	switch c {
	case CategoryMotion:
		return MeaningDetected, MeaningClear
	case CategoryDoor, CategoryWindow:
		return MeaningOpen, MeaningClosed
	case CategoryLeak:
		return MeaningWet, MeaningDry
	case CategoryBatteryLow:
		return MeaningLow, MeaningNormal
	default:
		return MeaningInvalid, MeaningInvalid
	}
}

func interpretNumber(c Category, raw string) Interpretation {
	// Only plain decimal numbers: ParseFloat would also accept hex floats and
	// digit separators, which Home Assistant never sends.
	if raw == "" || strings.ContainsAny(raw, "xXpP_") {
		return Interpretation{Meaning: MeaningInvalid}
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return Interpretation{Meaning: MeaningInvalid}
	}
	if (c == CategoryHumidity || c == CategoryBattery) && (v < 0 || v > 100) {
		return Interpretation{Meaning: MeaningInvalid}
	}
	return Interpretation{Meaning: MeaningReading, Value: v, HasValue: true}
}
