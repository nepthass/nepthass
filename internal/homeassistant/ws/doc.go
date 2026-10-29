// Package ws speaks the Home Assistant WebSocket API for one connection: the
// authentication handshake, command and result correlation by id, event
// delivery and liveness pings. It knows nothing about sensors; the monitor
// package owns reconnection and state.
//
// The access token is sent exactly once, in the auth message. It is never
// logged, never placed in a URL and never included in an error.
package ws
