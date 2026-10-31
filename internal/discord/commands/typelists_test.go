package commands

import (
	"strings"
	"testing"

	"github.com/nepthass/nepthass/internal/monitor"
)

func TestTypeList_isDerivedFromTheRegistry_soNoRegisteredClassCanBeOmitted(t *testing.T) {
	all := typeList(0)
	for _, s := range monitor.Specs() {
		if !strings.Contains(all, strings.ToLower(s.Label)) {
			t.Errorf("%q is registered but missing from %q", s.Label, all)
		}
	}
	for _, want := range []string{"garage door", "opening", "water leak"} {
		if !strings.Contains(typeList(monitor.CapState), want) {
			t.Errorf("state-rule types must include %q: %q", want, typeList(monitor.CapState))
		}
	}
	if strings.Contains(typeList(monitor.CapState), "temperature") || !strings.Contains(typeList(monitor.CapThreshold), "temperature") {
		t.Error("state and threshold lists must follow the declared capabilities")
	}
}
