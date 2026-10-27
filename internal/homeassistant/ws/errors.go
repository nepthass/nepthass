package ws

import (
	"errors"
	"fmt"
)

var (
	// ErrAuthInvalid means Home Assistant rejected the access token.
	ErrAuthInvalid = errors.New("home assistant rejected the websocket access token")
	// ErrProtocol means the peer violated the protocol: malformed JSON, an
	// unexpected handshake message, or a message over the size limit.
	ErrProtocol = errors.New("websocket protocol violation")
	// ErrTransport means the connection failed or was lost.
	ErrTransport = errors.New("websocket transport failure")
	// ErrTimeout means the peer stopped answering.
	ErrTimeout = errors.New("websocket peer timed out")
	// ErrClosed means the session was closed locally.
	ErrClosed = errors.New("websocket session closed")
	// ErrCommandRejected means Home Assistant answered a command with success=false.
	ErrCommandRejected = errors.New("home assistant rejected the command")
)

// RejectedError carries Home Assistant's error for a failed command. Its
// message is Home Assistant's own text and never contains NeptHass secrets.
type RejectedError struct {
	Code    string
	Message string
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("%v: %s", ErrCommandRejected, e.Code)
}

// Is makes errors.Is(err, ErrCommandRejected) work.
func (e *RejectedError) Is(target error) bool { return target == ErrCommandRejected }
