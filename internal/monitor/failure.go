package monitor

import (
	"context"
	"errors"

	"github.com/nepthass/nepthass/internal/homeassistant/ws"
)

// Failure is the category of the last connection failure. It is shown to
// users (never the underlying error) and selects the retry policy.
type Failure string

const (
	FailureNone           Failure = ""
	FailureNetwork        Failure = "network"
	FailureAuthentication Failure = "authentication"
	FailureProtocol       Failure = "protocol"
)

// errSyncOverflow means more events than the buffer holds arrived while the
// snapshot was being fetched. The attempt is abandoned and retried.
var errSyncOverflow = errors.New("event buffer overflowed during synchronization")

// classifyFailure maps an error that ended a session onto a Failure.
func classifyFailure(err error) Failure {
	switch {
	case err == nil:
		return FailureNone
	case errors.Is(err, ws.ErrAuthInvalid):
		return FailureAuthentication
	case errors.Is(err, ws.ErrProtocol), errors.Is(err, ws.ErrCommandRejected), errors.Is(err, errSyncOverflow), errors.Is(err, ErrInvalid):
		return FailureProtocol
	case errors.Is(err, ws.ErrTransport), errors.Is(err, ws.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return FailureNetwork
	default:
		return FailureNetwork
	}
}
