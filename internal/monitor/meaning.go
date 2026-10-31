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

// Interpret derives the meaning of a raw state from the category's registry
// row. For a binary class "on" and "off" have the meanings Home Assistant
// documents for it; anything else is Invalid. A category that is not in the
// registry has no meaning: every state other than unavailable and unknown is Invalid.
func Interpret(c Category, raw string) Interpretation {
	switch raw {
	case "unavailable":
		return Interpretation{Meaning: MeaningUnavailable}
	case "unknown":
		return Interpretation{Meaning: MeaningUnknown}
	}
	spec, ok := SpecFor(c)
	if !ok {
		return Interpretation{Meaning: MeaningInvalid}
	}
	if spec.Value == ValueNumeric {
		return interpretNumber(spec, raw)
	}
	switch raw {
	case "on":
		return Interpretation{Meaning: spec.On}
	case "off":
		return Interpretation{Meaning: spec.Off}
	}
	return Interpretation{Meaning: MeaningInvalid}
}

func interpretNumber(spec Spec, raw string) Interpretation {
	// Only plain decimal numbers: ParseFloat would also accept hex floats and
	// digit separators, which Home Assistant never sends.
	if raw == "" || strings.ContainsAny(raw, "xXpP_") {
		return Interpretation{Meaning: MeaningInvalid}
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return Interpretation{Meaning: MeaningInvalid}
	}
	if spec.Range != nil && (v < spec.Range.Min || v > spec.Range.Max) {
		return Interpretation{Meaning: MeaningInvalid}
	}
	return Interpretation{Meaning: MeaningReading, Value: v, HasValue: true}
}
