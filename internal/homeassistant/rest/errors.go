package rest

import (
	"errors"
	"fmt"
)

var (
	// ErrUnauthorized means Home Assistant rejected the access token.
	ErrUnauthorized = errors.New("home assistant rejected the access token")
	// ErrNotFound means the requested entity or service does not exist.
	ErrNotFound = errors.New("home assistant resource not found")
	// ErrUnavailable means Home Assistant could not be reached in time.
	ErrUnavailable = errors.New("home assistant unreachable")
	// ErrServiceNotAllowed means the call is outside the control allowlist.
	ErrServiceNotAllowed = errors.New("service call is not allowed")
)

// APIError is an unexpected HTTP status from Home Assistant.
type APIError struct {
	Op     string
	Status int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: unexpected home assistant status %d", e.Op, e.Status)
}
