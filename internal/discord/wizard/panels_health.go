package wizard

import (
	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/discord/ui"
	"github.com/nepthass/nepthass/internal/setup"
)

// healthPanel is step 1: real connection health, with a retry.
func healthPanel(sess setup.Session, rep commands.HealthReport, version, notice string) commands.Reply {
	gateway := ui.IconOK + " Connected"
	if !rep.Gateway.Connected {
		gateway = ui.IconFail + " Not connected"
	}
	check := func(err error, okText string) string {
		if err == nil {
			return ui.IconOK + " " + okText
		}
		return ui.IconFail + " " + commands.ProbeText(err)
	}
	color := ui.ColorOK
	if rep.HAErr != nil || rep.DBErr != nil {
		color = ui.ColorWarn
	}
	e := ui.Embed("Setup", progress(sess.Step, "Connection health")+"\nNothing is changed until you confirm at the end. Credentials are never entered in Discord.", color)
	e.Fields = []*discordgo.MessageEmbedField{
		ui.Field("Discord gateway", gateway, true),
		ui.Field("Home Assistant REST API", check(rep.HAErr, "Connected (Home Assistant "+ui.Escape(rep.HAVersion)+")"), true),
		ui.Field("SQLite database", check(rep.DBErr, "Responding"), true),
		ui.Field("Home Assistant WebSocket", ui.IconDisabled+" Not available in this release", true),
		ui.Field("NeptHass version", ui.Escape(version), true),
	}
	if rep.HAErr != nil {
		e.Fields = append(e.Fields, ui.Field("What to do", "Home Assistant must be reachable before anything can be configured: setup never uses stale or invented entity data. Check that it is running and press Retry.", false))
	}
	next := button(sess, actNext, "Next \u25B6", discordgo.PrimaryButton, rep.HAErr == nil && rep.DBErr == nil)
	return commands.Reply{Content: notice, Embeds: []*discordgo.MessageEmbed{e}, Components: []discordgo.MessageComponent{
		row(
			button(sess, actRetry, "Retry checks", discordgo.SecondaryButton, true),
			next,
			button(sess, actCancel, "Cancel setup", discordgo.DangerButton, true),
		),
	}}
}
