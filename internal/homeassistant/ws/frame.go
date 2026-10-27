package ws

import (
	"encoding/json"
	"fmt"
)

// Message types of the Home Assistant WebSocket API.
const (
	typeAuthRequired = "auth_required"
	typeAuth         = "auth"
	typeAuthOK       = "auth_ok"
	typeAuthInvalid  = "auth_invalid"
	typeResult       = "result"
	typeEvent        = "event"
	typePing         = "ping"
	typePong         = "pong"
)

// frame is the union of every message Home Assistant sends. Only the fields
// NeptHass uses are decoded; result and event payloads stay raw so callers
// decode exactly what they need.
type frame struct {
	ID        int64           `json:"id"`
	Type      string          `json:"type"`
	Success   *bool           `json:"success"`
	Result    json.RawMessage `json:"result"`
	Error     *frameError     `json:"error"`
	Event     json.RawMessage `json:"event"`
	Message   string          `json:"message"`
	HAVersion string          `json:"ha_version"`
}

type frameError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// decodeFrame parses one text frame. A frame that is not a JSON object with a
// type is a protocol violation. The error never includes the frame bytes:
// they could contain data the log must not carry.
func decodeFrame(data []byte) (frame, error) {
	var f frame
	if err := json.Unmarshal(data, &f); err != nil {
		return frame{}, fmt.Errorf("%w: frame is not valid json", ErrProtocol)
	}
	if f.Type == "" {
		return frame{}, fmt.Errorf("%w: frame has no type", ErrProtocol)
	}
	return f, nil
}

// command is an outgoing request: the type, a strictly increasing id and any
// extra fields.
func command(id int64, typ string, params map[string]any) map[string]any {
	msg := map[string]any{"id": id, "type": typ}
	for k, v := range params {
		if k != "id" && k != "type" {
			msg[k] = v
		}
	}
	return msg
}
