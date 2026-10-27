package discord

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bwmarrin/discordgo"
)

// routeLibraryLogs sends discordgo's internal log lines through slog (and
// therefore through secret redaction). discordgo exposes only a package-level
// hook, so this is the one place the application touches global state.
func routeLibraryLogs(log *slog.Logger) {
	discordgo.Logger = func(level, _ int, format string, args ...any) {
		lvl := slog.LevelError
		switch level {
		case discordgo.LogDebug:
			lvl = slog.LevelDebug
		case discordgo.LogInformational:
			lvl = slog.LevelInfo
		case discordgo.LogWarning:
			lvl = slog.LevelWarn
		}
		log.Log(context.Background(), lvl, fmt.Sprintf(format, args...), "component", "discordgo")
	}
}
