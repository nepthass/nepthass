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

const (
	domainAll    = "*"
	domainShared = "@shared"
	maxDomains   = 23 // plus "All" and "Shared only" makes Discord's 25 options
)

// discoverPanel is step 2: browse, search, filter and select Home Assistant entities.
func discoverPanel(sess setup.Session, v setup.DiscoverView, notice string) commands.Reply {
	view := sess.Draft.View
	var b strings.Builder
	b.WriteString(progress(sess.Step, "Discover devices") + "\n")
	fmt.Fprintf(&b, "Home Assistant: %s Connected \u00b7 Discovered: **%d** %s \u00b7 Matches: **%d** \u00b7 Selected: **%d**\n", ui.IconOK, v.Total, ui.Entity(v.Total), v.Matches, v.Selected)
	fmt.Fprintf(&b, "Updated %s", ui.Relative(v.FetchedAt))
	var filters []string
	if view.Query != "" {
		filters = append(filters, "search \""+ui.Escape(view.Query)+"\"")
	}
	if view.Domain != "" {
		filters = append(filters, "domain `"+view.Domain+"`")
	}
	if view.Shared {
		filters = append(filters, "shared only")
	}
	if len(filters) > 0 {
		b.WriteString("\nFilters: " + strings.Join(filters, " \u00b7 "))
	}
	b.WriteString("\n\n")
	switch {
	case v.Total == 0:
		b.WriteString("Home Assistant reports no entities yet. Add devices there, then press **Refresh**.")
	case len(v.Items) == 0:
		b.WriteString("No entities match these filters. Change the search or filter, or press **Clear**.")
	}
	for _, it := range v.Items {
		b.WriteString(discoverLine(it) + "\n")
	}
	if v.Skipped > 0 {
		fmt.Fprintf(&b, "\n%s %d %s cannot be offered here: the ID is longer than 100 characters.", ui.IconWarn, v.Skipped, ui.Entity(v.Skipped))
	}
	e := ui.Embed("Setup", b.String(), ui.ColorOK)
	e.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("Page %d of %d. Tick entities in the menu, then press Next. Nothing is applied yet.", v.Page, v.Pages)}

	var rows []discordgo.MessageComponent
	if len(v.Items) > 0 {
		opts := make([]discordgo.SelectMenuOption, 0, len(v.Items))
		for _, it := range v.Items {
			opts = append(opts, option(displayName(it), fmt.Sprintf("%s \u00b7 %s", it.ID, it.State), string(it.ID), it.Selected))
		}
		rows = append(rows, row(menu(sess, actSelect, "Select entities to configure on this page", opts, true)))
	}
	rows = append(rows,
		row(menu(sess, actDomain, "Filter by domain", domainOptions(view, v), false)),
		row(
			button(sess, actPagePrev, "\u2039 Page", discordgo.SecondaryButton, v.Page > 1),
			button(sess, actPageNext, "Page \u203a", discordgo.SecondaryButton, v.Page < v.Pages),
			button(sess, actSearch, "Search\u2026", discordgo.PrimaryButton, v.Total > 0),
			button(sess, actRefresh, "Refresh", discordgo.SecondaryButton, true),
			button(sess, actClear, "Clear search", discordgo.SecondaryButton, view.Query != ""),
		),
		navRow(sess, true, "Next \u25B6", v.Selected > 0),
	)
	return commands.Reply{Content: notice, Embeds: []*discordgo.MessageEmbed{e}, Components: rows}
}

func displayName(it setup.DiscoverItem) string {
	if it.Alias != "" {
		return it.Alias
	}
	return it.Name
}

func discoverLine(it setup.DiscoverItem) string {
	box := "\u2610"
	if it.Selected {
		box = "\u2611"
	}
	state := ui.DescribeReading(string(it.ID.Domain()), it.DeviceClass, it.State, it.Unit, it.Reliable())
	if it.Gone {
		state = ui.IconWarn + " no longer in Home Assistant"
	}
	line := fmt.Sprintf("%s %s **%s** `%s`\n\u2003%s \u00b7 %s %s", box, ui.ReadingIcon(it.Reliable() && !it.Gone), ui.Escape(displayName(it)), it.ID, state, accessIcon(it.Access), accessLabel(it.Access))
	if it.DeviceClass != "" {
		line += " \u00b7 " + ui.Escape(it.DeviceClass)
	}
	if it.Alias != "" && it.Alias != it.Name {
		line += " \u00b7 HA name: " + ui.Escape(it.Name)
	}
	return line
}

func domainOptions(view setup.View, v setup.DiscoverView) []discordgo.SelectMenuOption {
	opts := []discordgo.SelectMenuOption{
		option("All domains", "Show every entity", domainAll, view.Domain == "" && !view.Shared),
		option("Shared only", "Entities already shared with NeptHass users", domainShared, view.Shared),
	}
	for i, d := range v.Domains {
		if i == maxDomains {
			break
		}
		opts = append(opts, option(string(d.Domain), fmt.Sprintf("%d %s", d.Count, ui.Entity(d.Count)), string(d.Domain), view.Domain == string(d.Domain) && !view.Shared))
	}
	return opts
}

// parseDomainChoice validates the domain menu value.
func parseDomainChoice(value string) (domain entities.Domain, shared, ok bool) {
	switch value {
	case domainAll:
		return "", false, true
	case domainShared:
		return "", true, true
	}
	if _, err := entities.ParseID(value + ".x"); err != nil {
		return "", false, false
	}
	return entities.Domain(value), false, true
}
