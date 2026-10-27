// Package wizard renders the /setup wizard as Discord embeds, buttons, select
// menus and modals, and turns clicks back into calls on the setup service. It
// holds no business rules: every transition, validation and write happens in
// internal/setup, and every component payload is treated as untrusted input.
package wizard

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

const (
	idPrefix = "nh"
	sidBytes = 16
)

// action is a whitelisted component action. Anything else in a custom ID is rejected.
type action string

const (
	actNext        action = "next"
	actBack        action = "back"
	actCancel      action = "cancel"
	actConfirm     action = "confirm"
	actRetry       action = "retry"
	actResume      action = "resume"
	actRestart     action = "restart"
	actRefresh     action = "refresh"
	actPagePrev    action = "pgprev"
	actPageNext    action = "pgnext"
	actSearch      action = "search"
	actClear       action = "clear"
	actSelect      action = "sel"
	actDomain      action = "dom"
	actFocus       action = "focus"
	actRead        action = "read"
	actControl     action = "ctrl"
	actNone        action = "none"
	actBulkRead    action = "bulkread"
	actBulkControl action = "bulkctrl"
	actBulkClear   action = "bulkclr"
	actAlias       action = "alias"
	actUnalias     action = "unalias"
	actSearchForm  action = "searchm"
	actAliasForm   action = "aliasm"
)

var known = map[action]struct{}{
	actNext: {}, actBack: {}, actCancel: {}, actConfirm: {}, actRetry: {}, actResume: {}, actRestart: {}, actRefresh: {},
	actPagePrev: {}, actPageNext: {}, actSearch: {}, actClear: {}, actSelect: {}, actDomain: {}, actFocus: {},
	actRead: {}, actControl: {}, actNone: {}, actBulkRead: {}, actBulkControl: {}, actBulkClear: {}, actAlias: {},
	actUnalias: {}, actSearchForm: {}, actAliasForm: {},
}

// customID identifies one component: the session, the session version the
// panel was rendered for, and the action. A click whose version is not the
// session's current version is stale and only causes a refresh.
type customID struct {
	sid string
	ver int
	act action
}

func (c customID) String() string { return fmt.Sprintf("%s:%s:%d:%s", idPrefix, c.sid, c.ver, c.act) }

// owns reports whether a custom ID belongs to the wizard.
func owns(raw string) bool { return strings.HasPrefix(raw, idPrefix+":") }

// parseID validates the shape of a custom ID. It proves nothing about
// permission: the session is loaded and authorized separately.
func parseID(raw string) (customID, bool) {
	parts := strings.Split(raw, ":")
	if len(parts) != 4 || parts[0] != idPrefix {
		return customID{}, false
	}
	if b, err := hex.DecodeString(parts[1]); err != nil || len(b) != sidBytes {
		return customID{}, false
	}
	ver, err := strconv.Atoi(parts[2])
	if err != nil || ver < 1 {
		return customID{}, false
	}
	a := action(parts[3])
	if _, ok := known[a]; !ok {
		return customID{}, false
	}
	return customID{sid: parts[1], ver: ver, act: a}, true
}

// opensModal reports whether an action must be answered with a modal.
func opensModal(a action) bool { return a == actSearch || a == actAlias }
