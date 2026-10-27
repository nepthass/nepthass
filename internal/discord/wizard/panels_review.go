package wizard

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/discord/ui"
	"github.com/nepthass/nepthass/internal/setup"
)

const maxSectionRunes = 900

// section renders bullet lines, cutting the list to fit an embed field.
func section(lines []string) string {
	var b strings.Builder
	for i, l := range lines {
		if b.Len()+len(l) > maxSectionRunes {
			fmt.Fprintf(&b, "\u2026 and %d more", len(lines)-i)
			break
		}
		b.WriteString(l + "\n")
	}
	return strings.TrimSpace(b.String())
}

func ref(e setup.PlanEntry) string {
	return fmt.Sprintf("**%s** `%s`", ui.Escape(e.Name), e.ID)
}

// reviewPanel is step 5: the complete diff, and the only place changes are confirmed.
func reviewPanel(sess setup.Session, plan setup.Plan, notice string) commands.Reply {
	var added, access, revoked, aliases, problems []string
	for _, e := range plan.Entries {
		switch e.Kind {
		case setup.KindNew:
			added = append(added, fmt.Sprintf("\u2022 %s \u2192 %s", ref(e), accessLabel(e.Desired.Access)))
		case setup.KindRevoked:
			revoked = append(revoked, fmt.Sprintf("\u2022 %s (was %s)", ref(e), accessLabel(e.Current.Access)))
		case setup.KindModified:
			if e.Current.Access != e.Desired.Access {
				access = append(access, fmt.Sprintf("\u2022 %s: %s \u2192 %s", ref(e), accessLabel(e.Current.Access), accessLabel(e.Desired.Access)))
			}
		case setup.KindUnchanged:
		}
		if e.Alias != setup.AliasUnchanged {
			aliases = append(aliases, fmt.Sprintf("\u2022 %s: %s \u2192 %s", ref(e), quoted(e.Current.Alias), quoted(e.Desired.Alias)))
		}
		for _, p := range e.Problems {
			problems = append(problems, fmt.Sprintf("\u2022 `%s`: %s", e.ID, ui.Escape(p)))
		}
	}
	color := ui.ColorOK
	if len(problems) > 0 {
		color = ui.ColorWarn
	}
	desc := progress(sess.Step, "Review") + "\nThis is exactly what will change. Nothing has been applied yet."
	if plan.Revoked > 0 {
		desc += fmt.Sprintf("\n%s **%d %s will lose access.**", ui.IconWarn, plan.Revoked, ui.Entity(plan.Revoked))
	}
	e := ui.Embed("Setup", desc, color)
	addField := func(name string, lines []string) {
		if len(lines) > 0 {
			e.Fields = append(e.Fields, ui.Field(fmt.Sprintf("%s (%d)", name, len(lines)), section(lines), false))
		}
	}
	addField("Newly shared", added)
	addField("Permissions modified", access)
	addField("Access revoked", revoked)
	addField("Alias changes", aliases)
	addField(ui.IconWarn+" Blocking problems", problems)
	e.Fields = append(e.Fields, ui.Field("Not changed", fmt.Sprintf("Selected entities that already match: %d. Other shared entities left untouched: %d.", plan.Unchanged, plan.UntouchedShared), false))
	if len(problems) == 0 && plan.Changes() == 0 && plan.AliasChanges == 0 {
		e.Fields = append(e.Fields, ui.Field("Nothing to apply", "Every selected entity already matches the current configuration. Go back to change something, or cancel.", false))
	}
	canApply := len(problems) == 0 && (plan.Changes() > 0 || plan.AliasChanges > 0)
	return commands.Reply{Content: notice, Embeds: []*discordgo.MessageEmbed{e}, Components: []discordgo.MessageComponent{
		row(
			button(sess, actBack, "\u25C0 Back", discordgo.SecondaryButton, true),
			button(sess, actConfirm, "Confirm and apply", discordgo.SuccessButton, canApply),
			button(sess, actCancel, "Cancel setup", discordgo.DangerButton, true),
		),
	}}
}

func quoted(alias string) string {
	if alias == "" {
		return "_none_"
	}
	return "\"" + ui.Escape(alias) + "\""
}

// donePanel is step 6: what was applied and what to try next.
func donePanel(r setup.Result) commands.Reply {
	e := ui.Embed("Setup complete", fmt.Sprintf("%s Your configuration was saved and is active now. No restart is needed.", ui.IconOK), ui.ColorOK)
	e.Fields = []*discordgo.MessageEmbedField{
		ui.Field("Applied", fmt.Sprintf("%d newly shared, %d modified, %d revoked, %d alias changes", r.New, r.Modified, r.Revoked, r.AliasChanges), false),
		ui.Field("Try it", "`/devices` lists what is shared, `/status` shows an entity, and `/control` switches a shared light or switch. `/audit` records what setup changed.", false),
	}
	return commands.Reply{Embeds: []*discordgo.MessageEmbed{e}, Components: []discordgo.MessageComponent{}}
}

func messagePanel(title, text string, color int) commands.Reply {
	return commands.Reply{Embeds: []*discordgo.MessageEmbed{ui.Embed(title, text, color)}, Components: []discordgo.MessageComponent{}}
}

func cancelledPanel() commands.Reply {
	return messagePanel("Setup cancelled", "Nothing was changed. Run `/setup` to start again.", ui.ColorWarn)
}

func expiredPanel() commands.Reply {
	return messagePanel("Setup expired", "This setup session sat idle for too long and was closed. Nothing was changed. Run `/setup` to start again.", ui.ColorWarn)
}

func finishedPanel() commands.Reply {
	return messagePanel("Setup finished", "This setup session is already finished. Run `/setup` to start a new one.", ui.ColorWarn)
}

func appliedPanel() commands.Reply {
	return messagePanel("Setup already applied", ui.IconOK+" This setup was already applied; nothing was written a second time. Use `/devices` to see the result.", ui.ColorOK)
}

func deniedPanel() commands.Reply {
	return commands.Reply{Content: ui.IconFail + " You are not authorized to use NeptHass setup.", Embeds: []*discordgo.MessageEmbed{}, Components: []discordgo.MessageComponent{}}
}

// unavailablePanel is shown when Home Assistant cannot supply live data.
func unavailablePanel(sess setup.Session) commands.Reply {
	e := ui.Embed("Setup", ui.IconFail+" Home Assistant is not reachable right now.\nSetup never continues on stale or invented entity data. Check that Home Assistant is running, then retry. Your progress is saved.", ui.ColorError)
	return commands.Reply{Embeds: []*discordgo.MessageEmbed{e}, Components: []discordgo.MessageComponent{
		row(
			button(sess, actRetry, "Retry", discordgo.PrimaryButton, true),
			button(sess, actCancel, "Cancel setup", discordgo.DangerButton, true),
		),
	}}
}

// resumePanel offers to continue an interrupted session or start over.
func resumePanel(sess setup.Session) commands.Reply {
	text := fmt.Sprintf("You have a setup in progress: **step %d of 6**, **%d** %s selected. Nothing from it has been applied.", sess.Step.Number(), len(sess.Draft.Entries), ui.Entity(len(sess.Draft.Entries)))
	e := ui.Embed("Setup", text, ui.ColorOK)
	return commands.Reply{Embeds: []*discordgo.MessageEmbed{e}, Components: []discordgo.MessageComponent{
		row(
			button(sess, actResume, "Resume", discordgo.PrimaryButton, true),
			button(sess, actRestart, "Start over", discordgo.SecondaryButton, true),
			button(sess, actCancel, "Cancel setup", discordgo.DangerButton, true),
		),
	}}
}
