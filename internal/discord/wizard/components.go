package wizard

import (
	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/discord/ui"
	"github.com/nepthass/nepthass/internal/setup"
)

// Discord's component limits, kept in one place and asserted by the tests.
const (
	maxRows         = 5
	maxButtonsInRow = 5
	maxSelectOpts   = 25
	maxLabel        = 80
	maxOptionText   = 100
)

func id(sess setup.Session, a action) string {
	return customID{sid: sess.ID, ver: sess.Version, act: a}.String()
}

func button(sess setup.Session, a action, label string, style discordgo.ButtonStyle, enabled bool) discordgo.Button {
	return discordgo.Button{Label: ui.Truncate(label, maxLabel), Style: style, Disabled: !enabled, CustomID: id(sess, a)}
}

func row(components ...discordgo.MessageComponent) discordgo.ActionsRow {
	return discordgo.ActionsRow{Components: components}
}

func option(label, description, value string, selected bool) discordgo.SelectMenuOption {
	return discordgo.SelectMenuOption{
		Label: ui.Truncate(label, maxOptionText), Description: ui.Truncate(description, maxOptionText), Value: value, Default: selected,
	}
}

// menu builds a string select menu. min 0 lets the user deselect everything.
func menu(sess setup.Session, a action, placeholder string, options []discordgo.SelectMenuOption, multi bool) discordgo.SelectMenu {
	minValues, maxValues := 0, 1
	if multi {
		maxValues = len(options)
	}
	return discordgo.SelectMenu{
		MenuType: discordgo.StringSelectMenu, CustomID: id(sess, a), Placeholder: ui.Truncate(placeholder, maxOptionText),
		MinValues: &minValues, MaxValues: maxValues, Options: options,
	}
}

// navRow is the Back / Next / Cancel row every step ends with.
func navRow(sess setup.Session, backOK bool, nextLabel string, nextOK bool) discordgo.ActionsRow {
	return row(
		button(sess, actBack, "\u25C0 Back", discordgo.SecondaryButton, backOK),
		button(sess, actNext, nextLabel, discordgo.PrimaryButton, nextOK),
		button(sess, actCancel, "Cancel setup", discordgo.DangerButton, true),
	)
}

// progress renders "Step N of 6" with a bar.
func progress(step setup.Step, title string) string {
	n := step.Number()
	bar := ""
	for i := 1; i <= 6; i++ {
		if i <= n {
			bar += "\u25A0"
		} else {
			bar += "\u25A1"
		}
	}
	return "**Step " + itoa(n) + " of 6: " + title + "**  " + bar
}
