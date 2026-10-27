// Package logging builds the application's structured logger.
package logging

import (
	"io"
	"log/slog"
	"strings"

	"github.com/nepthass/nepthass/internal/config"
)

const redacted = "[REDACTED]"

// New returns a slog.Logger that writes to w in the configured format and
// scrubs every occurrence of the given secrets from messages, string
// attributes and error values. It is a safety net behind config.Secret, which
// already redacts itself: it catches secrets embedded in third-party error
// strings or log lines.
func New(w io.Writer, cfg config.Log, secrets []config.Secret) *slog.Logger {
	pairs := make([]string, 0, 2*len(secrets))
	for _, s := range secrets {
		if raw := s.Reveal(); raw != "" {
			pairs = append(pairs, raw, redacted)
		}
	}
	scrub := strings.NewReplacer(pairs...).Replace
	opts := &slog.HandlerOptions{
		Level: cfg.Level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			v := a.Value.Resolve()
			switch v.Kind() {
			case slog.KindString:
				a.Value = slog.StringValue(scrub(v.String()))
			case slog.KindAny:
				if err, ok := v.Any().(error); ok {
					a.Value = slog.StringValue(scrub(err.Error()))
				}
			default:
				// Other kinds cannot carry a secret string.
			}
			return a
		},
	}
	if cfg.Format == config.LogJSON {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
