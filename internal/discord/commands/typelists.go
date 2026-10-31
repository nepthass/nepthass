package commands

import (
	"strings"

	"github.com/nepthass/nepthass/internal/monitor"
)

// typeList names, in registry order, the sensor types whose rules need every capability in want
// (zero for all registered types), so user-facing lists can never drift from the registry.
func typeList(want monitor.Caps) string {
	var out []string
	for _, s := range monitor.Specs() {
		if s.Caps.Has(want) {
			out = append(out, strings.ToLower(s.Label))
		}
	}
	switch len(out) {
	case 0:
		return "no"
	case 1:
		return out[0]
	}
	return strings.Join(out[:len(out)-1], ", ") + " or " + out[len(out)-1]
}
