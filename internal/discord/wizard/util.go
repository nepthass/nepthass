package wizard

import (
	"strconv"

	"github.com/nepthass/nepthass/internal/entities"
)

func itoa(n int) string { return strconv.Itoa(n) }

func accessLabel(a entities.Access) string {
	switch a {
	case entities.AccessControl:
		return "control"
	case entities.AccessRead:
		return "read"
	default:
		return "no access"
	}
}

func accessIcon(a entities.Access) string {
	switch a {
	case entities.AccessControl:
		return "\U0001F39B"
	case entities.AccessRead:
		return "\U0001F441"
	default:
		return "\u2796"
	}
}
