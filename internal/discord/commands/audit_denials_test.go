package commands

import (
	"strings"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/discord/ui"
)

// What an administrator reads in /audit for a refused command: who, which command and subcommand,
// the fixed reason and the number of rows dropped by the rate limit, and never an entity or a value.
func TestAuditLine_deniedRowsShowTheReasonAndTheDroppedCount_andNeverAnEntityOrAValue(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		entry  audit.Entry
		wanted []string
	}{
		{"a user who is not an administrator", audit.Entry{Time: at, ActorID: "600000000000000002", Command: "notifications", Action: "add",
			Outcome: audit.OutcomeDenied, Detail: "reason=not_admin"},
			[]string{ui.IconDenied, "<@600000000000000002>", "`notifications`", " add", `reason=not\_admin`}},
		{"a stranger whose earlier attempts were rate limited", audit.Entry{Time: at, ActorID: "600000000000000003", Command: "permissions", Action: "allow",
			Outcome: audit.OutcomeDenied, Detail: "reason=not_allowed suppressed=12"},
			[]string{ui.IconDenied, "`permissions`", `reason=not\_allowed`, "suppressed=12"}},
		{"another server", audit.Entry{Time: at, ActorID: "unknown", Command: "sync", Action: "",
			Outcome: audit.OutcomeDenied, Detail: "reason=wrong_guild guild=none"},
			[]string{ui.IconDenied, "`sync`", `reason=wrong\_guild`, "guild=none"}},
	}
	for _, c := range cases {
		line := auditLine(c.entry)
		for _, want := range c.wanted {
			if !strings.Contains(line, want) {
				t.Errorf("%s: %q must contain %q", c.name, line, want)
			}
		}
		for _, never := range []string{"binary_sensor", "sensor.", "`sensor", "above", "threshold"} {
			if strings.Contains(line, never) {
				t.Errorf("%s: a refused attempt shows no entity or option value, found %q in %q", c.name, never, line)
			}
		}
	}
}
