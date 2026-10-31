package monitor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"strconv"
	"testing"
	"time"
)

// legacyClassify, legacyInterpret, legacyLabel and legacyNumeric are a frozen
// copy of the v0.4.0 classification logic. The registry must reproduce them
// exactly for every class that existed in v0.4.0.
var legacyClasses = map[classKey]Category{
	{DomainSensor, "temperature"}:    CategoryTemperature,
	{DomainSensor, "humidity"}:       CategoryHumidity,
	{DomainSensor, "battery"}:        CategoryBattery,
	{DomainBinarySensor, "battery"}:  CategoryBatteryLow,
	{DomainBinarySensor, "motion"}:   CategoryMotion,
	{DomainBinarySensor, "door"}:     CategoryDoor,
	{DomainBinarySensor, "window"}:   CategoryWindow,
	{DomainBinarySensor, "moisture"}: CategoryLeak,
}

func legacyNumeric(c Category) bool {
	return c == CategoryTemperature || c == CategoryHumidity || c == CategoryBattery
}

func legacyLabel(c Category) string {
	return map[Category]string{CategoryTemperature: "Temperature", CategoryHumidity: "Humidity", CategoryBattery: "Battery", CategoryBatteryLow: "Battery low",
		CategoryMotion: "Motion", CategoryDoor: "Door", CategoryWindow: "Window", CategoryLeak: "Water leak"}[c]
}

func legacyInterpret(c Category, raw string) Interpretation {
	switch raw {
	case "unavailable":
		return Interpretation{Meaning: MeaningUnavailable}
	case "unknown":
		return Interpretation{Meaning: MeaningUnknown}
	}
	if legacyNumeric(c) {
		v, err := strconv.ParseFloat(raw, 64)
		bad := raw == "" || err != nil || math.IsNaN(v) || math.IsInf(v, 0)
		for _, r := range raw {
			if r == 'x' || r == 'X' || r == 'p' || r == 'P' || r == '_' {
				bad = true
			}
		}
		if bad || ((c == CategoryHumidity || c == CategoryBattery) && (v < 0 || v > 100)) {
			return Interpretation{Meaning: MeaningInvalid}
		}
		return Interpretation{Meaning: MeaningReading, Value: v, HasValue: true}
	}
	on, off := MeaningInvalid, MeaningInvalid
	switch c {
	case CategoryMotion:
		on, off = MeaningDetected, MeaningClear
	case CategoryDoor, CategoryWindow:
		on, off = MeaningOpen, MeaningClosed
	case CategoryLeak:
		on, off = MeaningWet, MeaningDry
	case CategoryBatteryLow:
		on, off = MeaningLow, MeaningNormal
	}
	switch raw {
	case "on":
		return Interpretation{Meaning: on}
	case "off":
		return Interpretation{Meaning: off}
	}
	return Interpretation{Meaning: MeaningInvalid}
}

func TestRegistry_reproducesTheV040ClassificationExactly(t *testing.T) {
	domains := []string{DomainSensor, DomainBinarySensor, "light", "switch", "lock", "climate", ""}
	classes := []string{"temperature", "humidity", "battery", "motion", "door", "window", "moisture", "", "smoke", "gas", "carbon_monoxide", "power", "presence", "unknown_class", "Door", " door"}
	states := []string{"on", "off", "ON", "unavailable", "unknown", "", "0", "21.5", "-5", "100", "100.1", "-0.1", "1e3", "0x10", "1_000", "NaN", "Inf", "-Inf", "abc", "open", "true", "  7 "}
	for _, d := range domains {
		for _, dc := range classes {
			want, wantOK := legacyClasses[classKey{d, dc}]
			got, gotOK := Classify(d, dc)
			if gotOK != wantOK || got != want {
				t.Fatalf("Classify(%q, %q) = %q,%v; v0.4.0 had %q,%v", d, dc, got, gotOK, want, wantOK)
			}
			if !wantOK {
				continue
			}
			if got.Numeric() != legacyNumeric(want) || got.Label() != legacyLabel(want) {
				t.Errorf("%s: numeric or label drifted", want)
			}
			for _, s := range states {
				if g, w := Interpret(got, s), legacyInterpret(want, s); g != w {
					t.Errorf("Interpret(%s, %q) = %+v; v0.4.0 gave %+v", want, s, g, w)
				}
			}
		}
	}
	// A category outside the registry has no meaning, exactly as before.
	for _, s := range states {
		if g, w := Interpret("", s), legacyInterpret("", s); g != w {
			t.Errorf("Interpret(\"\", %q) = %+v; v0.4.0 gave %+v", s, g, w)
		}
	}
}

func TestRegistry_newBinaryClassesFollowHomeAssistantsContract(t *testing.T) {
	for _, dc := range []string{"garage_door", "opening"} {
		c, ok := Classify(DomainBinarySensor, dc)
		if !ok {
			t.Fatalf("%s is not classified", dc)
		}
		if g := Interpret(c, "on"); g.Meaning != MeaningOpen {
			t.Errorf("%s: on must mean open (binary_sensor/const.py), got %q", dc, g.Meaning)
		}
		if g := Interpret(c, "off"); g.Meaning != MeaningClosed {
			t.Errorf("%s: off must mean closed, got %q", dc, g.Meaning)
		}
		if c.Hazard() || c.Numeric() {
			t.Errorf("%s is neither numeric nor a hazard", dc)
		}
		if _, ok := Classify(DomainSensor, dc); ok {
			t.Errorf("a sensor-domain %s must stay unclassified", dc)
		}
	}
}

func TestRegistry_safetyClassesAreNotSupportedAndMoistureIsNotGeneric(t *testing.T) {
	for _, dc := range []string{"smoke", "gas", "carbon_monoxide", "safety", "tamper", "problem"} {
		if _, ok := Classify(DomainBinarySensor, dc); ok {
			t.Errorf("%s must not be classified in v0.5.0 (a dedicated safety policy comes first)", dc)
		}
	}
	// binary_sensor moisture is wet/dry and a hazard; sensor moisture is a percentage (soil) and is not mapped.
	if c, ok := Classify(DomainBinarySensor, "moisture"); !ok || !c.Hazard() {
		t.Error("binary_sensor moisture is the explicit water-leak hazard")
	}
	if _, ok := Classify(DomainSensor, "moisture"); ok {
		t.Error("sensor moisture is a percentage and must stay unclassified")
	}
}

func TestRegistry_invariants(t *testing.T) {
	seenKey, seenCat := map[classKey]bool{}, map[Category]bool{}
	for _, s := range Specs() {
		if s.Category == "" || s.DeviceClass == "" || s.Label == "" || s.Noun == "" {
			t.Errorf("%+v: category, class, label and noun are required", s)
		}
		k := classKey{s.Domain, s.DeviceClass}
		if seenKey[k] || seenCat[s.Category] {
			t.Errorf("%s: duplicate key or category", s.Category)
		}
		seenKey[k], seenCat[s.Category] = true, true
		if !Monitored(s.Domain) {
			t.Errorf("%s: domain %q is not monitored", s.Category, s.Domain)
		}
		if s.Cooldown < time.Minute || s.Cooldown > 24*time.Hour {
			t.Errorf("%s: default cooldown %v is outside 1 minute to 24 hours", s.Category, s.Cooldown)
		}
		switch s.Value {
		case ValueBinary:
			if s.Domain != DomainBinarySensor {
				t.Errorf("%s: a binary class must be a binary_sensor", s.Category)
			}
			if s.AlertState != "on" && s.AlertState != "off" {
				t.Errorf("%s: alert state must be on or off", s.Category)
			}
			if s.On == "" || s.Off == "" || s.On == s.Off || s.Phrase == "" {
				t.Errorf("%s: both meanings (distinct) and the rule wording are required", s.Category)
			}
			if !s.Caps.Has(CapState) || s.Caps.Has(CapThreshold) || s.Caps.Has(CapThresholdDuration) || s.Caps.Has(CapHysteresis) {
				t.Errorf("%s: a binary class has state capabilities only", s.Category)
			}
			if s.AlertMeaning() == "" {
				t.Errorf("%s: no alert meaning", s.Category)
			}
		case ValueNumeric:
			if s.Domain != DomainSensor {
				t.Errorf("%s: a numeric class must be a sensor", s.Category)
			}
			if len(s.Units) == 0 {
				t.Errorf("%s: a numeric class declares its documented units", s.Category)
			}
			if !s.Caps.Has(CapThreshold) || s.Caps.Has(CapState) || s.Caps.Has(CapStateDuration) {
				t.Errorf("%s: a numeric class has threshold capabilities only", s.Category)
			}
			if s.AlertMeaning() != "" {
				t.Errorf("%s: a numeric class has no alert meaning", s.Category)
			}
		default:
			t.Errorf("%s: unknown value kind", s.Category)
		}
		if s.Hazard && (s.Caps.Has(CapStateDuration) || s.Caps.Has(CapThresholdDuration) || s.Caps.Has(CapHysteresis)) {
			t.Errorf("%s: a hazard is immediate-only: no duration and no hysteresis", s.Category)
		}
		if s.Hazard && s.Value != ValueBinary {
			t.Errorf("%s: only a binary state can be a hazard", s.Category)
		}
		if s.Range != nil && s.Range.Min >= s.Range.Max {
			t.Errorf("%s: empty range", s.Category)
		}
		for _, u := range s.Units {
			if u == "" {
				t.Errorf("%s: empty unit", s.Category)
			}
		}
	}
}

// Every Category constant must have a registry row, so a future developer cannot
// add a category without declaring its required behavior.
func TestRegistry_everyCategoryConstantHasARow(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "classify.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var consts []string
	ast.Inspect(file, func(n ast.Node) bool {
		gd, ok := n.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			return true
		}
		for _, sp := range gd.Specs {
			vs := sp.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Category" {
				continue
			}
			for _, v := range vs.Values {
				if bl, ok := v.(*ast.BasicLit); ok {
					s, _ := strconv.Unquote(bl.Value)
					consts = append(consts, s)
				}
			}
		}
		return true
	})
	if len(consts) < 10 {
		t.Fatalf("parsed only %d Category constants; the parser test is broken", len(consts))
	}
	for _, c := range consts {
		if _, ok := SpecFor(Category(c)); !ok {
			t.Errorf("category constant %q has no registry row", c)
		}
	}
	if len(consts) != len(Specs()) {
		t.Errorf("%d category constants but %d registry rows", len(consts), len(Specs()))
	}
}

func TestRegistry_unitsAndRanges(t *testing.T) {
	temp, _ := SpecFor(CategoryTemperature)
	for _, u := range []string{"°C", "°F", "K"} {
		if !temp.SupportsUnit(u) {
			t.Errorf("temperature must support %s", u)
		}
	}
	if temp.SupportsUnit("") || temp.SupportsUnit("C") || temp.SupportsUnit("celsius") {
		t.Error("temperature accepts only Home Assistant's documented units, exactly")
	}
	if _, single := temp.SingleUnit(); single {
		t.Error("temperature has several units, so a legacy rule cannot be bound without asking")
	}
	hum, _ := SpecFor(CategoryHumidity)
	if u, single := hum.SingleUnit(); !single || u != "%" {
		t.Error("humidity has exactly one unit: %")
	}
}
