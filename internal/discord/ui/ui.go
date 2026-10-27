// Package ui holds the Discord presentation helpers shared by the slash
// commands and the setup wizard: NeptHass branding, embed builders, and the
// escaping and truncation that keep text from Home Assistant from breaking out
// of a message (markdown, mentions) or exceeding Discord's limits.
package ui

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/entities"
)

const (
	TitlePrefix = "\U0001F531 NeptHass \u2014 "

	ColorOK    = 0x2B7FFF
	ColorWarn  = 0xF5A623
	ColorError = 0xD0021B

	IconOK       = "\u2705"
	IconDenied   = "\u26D4"
	IconWarn     = "\u26A0\uFE0F"
	IconFail     = "\u274C"
	IconDisabled = "\u2796"

	MaxDescription = 4000
	MaxFieldValue  = 1000
	MaxChoice      = 100
)

// Embed builds a branded embed.
func Embed(title, description string, color int) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Title:       TitlePrefix + title,
		Description: Truncate(description, MaxDescription),
		Color:       color,
	}
}

// Field builds an embed field with a bounded value.
func Field(name, value string, inline bool) *discordgo.MessageEmbedField {
	return &discordgo.MessageEmbedField{Name: name, Value: Truncate(value, MaxFieldValue), Inline: inline}
}

// Truncate shortens s to at most limit runes, ending in an ellipsis when cut.
func Truncate(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit-1]) + "\u2026"
}

// Escape neutralizes Discord markdown and mention syntax in text that comes
// from Home Assistant (friendly names are attacker-influenced strings).
func Escape(s string) string {
	return strings.NewReplacer(
		"\\", "\\\\", "*", "\\*", "_", "\\_", "~", "\\~", "`", "\\`",
		"|", "\\|", ">", "\\>", "#", "\\#", "[", "\\[", "<", "\\<", "@", "\\@",
	).Replace(s)
}

// Relative renders a time as a Discord relative timestamp.
func Relative(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return "<t:" + strconv.FormatInt(t.Unix(), 10) + ":R>"
}

// Choice builds an autocomplete choice with a bounded name.
func Choice(name, value string) *discordgo.ApplicationCommandOptionChoice {
	return &discordgo.ApplicationCommandOptionChoice{Name: Truncate(name, MaxChoice), Value: value}
}

// ReliableIcon is the check mark for a real value and a warning for unavailable or unknown.
func ReliableIcon(reliable bool) string {
	if !reliable {
		return IconWarn
	}
	return IconOK
}

// StateIcon is ReliableIcon for a state.
func StateIcon(s entities.State) string { return ReliableIcon(s.Reliable()) }

// DescribeValue renders a state value with its unit. Unavailable and unknown are
// spelled out so they are never mistaken for an inactive or safe state.
func DescribeValue(state, unit string, reliable bool) string {
	if !reliable {
		return state + " (not reporting)"
	}
	if unit != "" {
		return Escape(state + " " + unit)
	}
	return Escape(state)
}

// DescribeState is DescribeValue for a state.
func DescribeState(s entities.State) string { return DescribeValue(s.State, s.Unit(), s.Reliable()) }
