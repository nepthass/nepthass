package config

import "log/slog"

const redacted = "[REDACTED]"

// Secret holds a credential. Every formatting path (fmt verbs, slog, text and
// JSON marshalling) prints a placeholder; the raw value is only available
// through Reveal so that leaking it requires an explicit, greppable call.
type Secret string

// Reveal returns the raw credential. Call it only when handing the value to
// the library that authenticates with it.
func (s Secret) Reveal() string { return string(s) }

// String implements fmt.Stringer.
func (Secret) String() string { return redacted }

// GoString implements fmt.GoStringer so %#v does not leak the value either.
func (Secret) GoString() string { return redacted }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalText implements encoding.TextMarshaler (and therefore JSON).
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
