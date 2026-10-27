package wizard

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/discord/ui"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/setup"
)

func draftName(it setup.DraftItem) string {
	if it.Entry.Desired.Alias != "" {
		return it.Entry.Desired.Alias
	}
	return it.Name
}

func focusMenu(sess setup.Session, v setup.DraftView) discordgo.MessageComponent {
	opts := make([]discordgo.SelectMenuOption, 0, len(v.Items))
	for _, it := range v.Items {
		selected := v.Focus != nil && v.Focus.ID == it.ID
		opts = append(opts, option(draftName(it), string(it.ID), string(it.ID), selected))
	}
	return row(menu(sess, actFocus, "Pick an entity to edit", opts, false))
}

func pagerButtons(sess setup.Session, v setup.DraftView) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		button(sess, actPagePrev, "\u2039 Page", discordgo.SecondaryButton, v.Page > 1),
		button(sess, actPageNext, "Page \u203a", discordgo.SecondaryButton, v.Page < v.Pages),
	}
}

// permissionsPanel is step 3: read or control per selected entity.
func permissionsPanel(sess setup.Session, v setup.DraftView, notice string) commands.Reply {
	var b strings.Builder
	b.WriteString(progress(sess.Step, "Device permissions") + "\n")
	fmt.Fprintf(&b, "Selected: **%d** %s. New shares default to **read**. Control is available for lights and switches only.\n\n", v.Total, ui.Entity(v.Total))
	for _, it := range v.Items {
		mark := "\u2003"
		if v.Focus != nil && v.Focus.ID == it.ID {
			mark = "\u25B6 "
		}
		changed := ""
		if it.Entry.Desired.Access != it.Current.Access {
			changed = " \u2022"
		}
		warn := ""
		if it.Missing {
			warn = " " + ui.IconWarn + " not in Home Assistant"
		}
		fmt.Fprintf(&b, "%s%s **%s** `%s` \u2014 now: %s \u2192 **%s**%s%s\n", mark, accessIcon(it.Entry.Desired.Access), ui.Escape(draftName(it)), it.ID, accessLabel(it.Current.Access), accessLabel(it.Entry.Desired.Access), changed, warn)
	}
	e := ui.Embed("Setup", b.String(), ui.ColorOK)
	e.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("Page %d of %d. \u2022 marks a pending change. Nothing is applied yet.", v.Page, v.Pages)}

	haveFocus := v.Focus != nil
	canControl := haveFocus && v.Focus.ID.Domain().Controllable()
	accessRow := []discordgo.MessageComponent{
		button(sess, actRead, "Read", discordgo.PrimaryButton, haveFocus),
		button(sess, actControl, "Control", discordgo.PrimaryButton, canControl),
		button(sess, actNone, "No access", discordgo.DangerButton, haveFocus),
	}
	rows := []discordgo.MessageComponent{
		focusMenu(sess, v),
		row(append(accessRow, pagerButtons(sess, v)...)...),
		row(
			button(sess, actBulkRead, "All: read", discordgo.SecondaryButton, v.Total > 0),
			button(sess, actBulkControl, "Lights and switches: control", discordgo.SecondaryButton, v.Total > 0),
			button(sess, actBulkClear, "Clear all", discordgo.SecondaryButton, v.Total > 0),
		),
		navRow(sess, true, "Next \u25B6", true),
	}
	return commands.Reply{Content: notice, Embeds: []*discordgo.MessageEmbed{e}, Components: rows}
}

// aliasesPanel is step 4: human-readable aliases for the selected entities.
func aliasesPanel(sess setup.Session, v setup.DraftView, notice string) commands.Reply {
	var b strings.Builder
	b.WriteString(progress(sess.Step, "Aliases") + "\n")
	b.WriteString("An alias is a friendly name you can type in /status and /control. It never grants access by itself. Letters, numbers, spaces and - ' . ( ), up to 32 characters; unique across NeptHass.\n\n")
	for _, it := range v.Items {
		mark := "\u2003"
		if v.Focus != nil && v.Focus.ID == it.ID {
			mark = "\u25B6 "
		}
		alias := "_none_"
		if it.Entry.Desired.Alias != "" {
			alias = "**" + ui.Escape(it.Entry.Desired.Alias) + "**"
		}
		changed := ""
		if it.Entry.Desired.Alias != it.Current.Alias {
			changed = " \u2022"
		}
		fmt.Fprintf(&b, "%s%s `%s` \u2014 %s%s\n", mark, ui.Escape(it.Name), it.ID, alias, changed)
	}
	e := ui.Embed("Setup", b.String(), ui.ColorOK)
	e.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("Page %d of %d. Aliases are optional: press Next to skip.", v.Page, v.Pages)}

	haveFocus := v.Focus != nil
	hasAlias := haveFocus && v.Focus.Entry.Desired.Alias != ""
	rows := []discordgo.MessageComponent{
		focusMenu(sess, v),
		row(append([]discordgo.MessageComponent{
			button(sess, actAlias, "Set alias\u2026", discordgo.PrimaryButton, haveFocus),
			button(sess, actUnalias, "Remove alias", discordgo.DangerButton, hasAlias),
		}, pagerButtons(sess, v)...)...),
		navRow(sess, true, "Next \u25B6", true),
	}
	return commands.Reply{Content: notice, Embeds: []*discordgo.MessageEmbed{e}, Components: rows}
}

// aliasModal asks for the focused entity's alias.
func aliasModal(sess setup.Session, name, current string) *commands.Modal {
	return &commands.Modal{
		CustomID: id(sess, actAliasForm),
		Title:    ui.Truncate("Alias for "+name, 45),
		Inputs: []commands.ModalInput{{
			CustomID: "alias", Label: "Alias", Placeholder: "for example: Living Room", Value: current,
			Required: false, MaxLength: entities.MaxAliasRunes,
		}},
	}
}

// searchModal asks for discovery search words.
func searchModal(sess setup.Session) *commands.Modal {
	return &commands.Modal{
		CustomID: id(sess, actSearchForm),
		Title:    "Search entities",
		Inputs: []commands.ModalInput{{
			CustomID: "q", Label: "Words in the name, entity ID or device class", Placeholder: "for example: living room lamp",
			Value: sess.Draft.View.Query, Required: false, MaxLength: 64,
		}},
	}
}
