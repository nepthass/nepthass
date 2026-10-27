package logging

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/nepthass/nepthass/internal/config"
)

const sentinel = "tok-sentinel-abcdef123456"

func TestNew_scrubsSecrets_fromMessagesStringsAndErrors(t *testing.T) {
	for _, format := range []config.LogFormat{config.LogText, config.LogJSON} {
		t.Run(string(format), func(t *testing.T) {
			var buf bytes.Buffer
			log := New(&buf, config.Log{Level: slog.LevelInfo, Format: format}, []config.Secret{sentinel})

			log.Info("connecting with "+sentinel,
				"url", "http://ha.test/?token="+sentinel,
				"err", errors.New("401 for bearer "+sentinel),
			)

			out := buf.String()
			if strings.Contains(out, sentinel) {
				t.Fatalf("secret leaked: %s", out)
			}
			if strings.Count(out, "[REDACTED]") != 3 {
				t.Errorf("expected three redactions: %s", out)
			}
		})
	}
}

func TestNew_respectsConfiguredLevel(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, config.Log{Level: slog.LevelWarn, Format: config.LogText}, nil)

	log.Info("quiet")
	log.Warn("loud")

	out := buf.String()
	if strings.Contains(out, "quiet") || !strings.Contains(out, "loud") {
		t.Errorf("level filtering wrong: %q", out)
	}
}
