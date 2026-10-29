// Package monitor keeps NeptHass's live view of Home Assistant sensors.
//
// A Monitor owns one WebSocket connection at a time (internal/homeassistant/ws),
// a connection state machine with bounded, jittered reconnection, and a
// Tracker holding the latest state of every classified sensor. The Reader is
// the only way monitored data reaches a Discord user, and it re-checks the
// current permission at read time. See docs/design/sensor-monitoring.md.
package monitor
