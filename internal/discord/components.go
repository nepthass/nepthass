package discord

import (
	"context"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/discord/commands"
)

// Wizard is the setup wizard as the bot sees it.
type Wizard interface {
	commands.SetupStarter
	Owns(customID string) bool
	OpensModal(customID string) bool
	Modal(ctx context.Context, actor auth.Actor, guildID, customID string) commands.ModalResult
	Component(ctx context.Context, actor auth.Actor, guildID, customID string, values []string) commands.Reply
	Submit(ctx context.Context, actor auth.Actor, guildID, customID string, fields map[string]string) commands.Reply
}

const (
	componentBurst  = 30
	componentWindow = 10 * time.Second
)

// runComponent handles a button click or select-menu submission. The caller was
// already authorized as a guild member on an allowlist; the wizard re-checks
// administrator level and session ownership on every click, and the custom ID
// is untrusted input.
func (b *Bot) runComponent(ctx context.Context, api interactionAPI, i *discordgo.Interaction, actor auth.Actor) {
	data := i.MessageComponentData()
	if b.wizard == nil || !b.wizard.Owns(data.CustomID) {
		b.say(api, i, "That kind of interaction is not supported.")
		return
	}
	if !b.componentLimiter.Allow(actor.UserID) {
		b.say(api, i, "You are clicking too quickly. Wait a few seconds and try again.")
		return
	}
	if b.wizard.OpensModal(data.CustomID) {
		b.openModal(ctx, api, i, actor, data.CustomID)
		return
	}
	if !b.acknowledgeUpdate(api, i) {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	b.deliver(api, i, b.wizard.Component(runCtx, actor, i.GuildID, data.CustomID, data.Values))
}

// runModalSubmit handles a submitted text dialog.
func (b *Bot) runModalSubmit(ctx context.Context, api interactionAPI, i *discordgo.Interaction, actor auth.Actor) {
	data := i.ModalSubmitData()
	if b.wizard == nil || !b.wizard.Owns(data.CustomID) {
		b.say(api, i, "That kind of interaction is not supported.")
		return
	}
	if !b.componentLimiter.Allow(actor.UserID) {
		b.say(api, i, "You are submitting too quickly. Wait a few seconds and try again.")
		return
	}
	if !b.acknowledgeUpdate(api, i) {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	b.deliver(api, i, b.wizard.Submit(runCtx, actor, i.GuildID, data.CustomID, modalFields(data)))
}

// openModal answers a modal-opening click. Opening a modal is the one response
// that cannot be deferred, so it only does database work.
func (b *Bot) openModal(ctx context.Context, api interactionAPI, i *discordgo.Interaction, actor auth.Actor, customID string) {
	runCtx, cancel := context.WithTimeout(ctx, autocompleteTimeout)
	defer cancel()
	res := b.wizard.Modal(runCtx, actor, i.GuildID, customID)
	if res.Modal == nil {
		b.sayReply(api, i, res.Notice)
		return
	}
	inputs := make([]discordgo.MessageComponent, 0, len(res.Modal.Inputs))
	for _, in := range res.Modal.Inputs {
		inputs = append(inputs, discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{
			CustomID: in.CustomID, Label: in.Label, Style: discordgo.TextInputShort, Placeholder: in.Placeholder,
			Value: in.Value, Required: in.Required, MaxLength: in.MaxLength,
		}}})
	}
	resp := &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{CustomID: res.Modal.CustomID, Title: res.Modal.Title, Components: inputs},
	}
	if err := api.InteractionRespond(i, resp); err != nil {
		b.log.Warn("could not open modal", "error", err)
	}
}

// acknowledgeUpdate defers a component update within Discord's 3 second limit.
func (b *Bot) acknowledgeUpdate(api interactionAPI, i *discordgo.Interaction) bool {
	ack := &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate}
	if err := api.InteractionRespond(i, ack); err != nil {
		b.log.Warn("could not acknowledge component interaction", "error", err)
		return false
	}
	return true
}

// sayReply sends a reply as a new ephemeral message.
func (b *Bot) sayReply(api interactionAPI, i *discordgo.Interaction, reply commands.Reply) {
	resp := &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: reply.Content, Embeds: reply.Embeds, Components: reply.Components, Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: noMentions()},
	}
	if err := api.InteractionRespond(i, resp); err != nil {
		b.log.Warn("could not respond to interaction", "error", err)
	}
}

// modalFields flattens the submitted text inputs by custom ID.
func modalFields(data discordgo.ModalSubmitInteractionData) map[string]string {
	fields := map[string]string{}
	for _, c := range data.Components {
		row, ok := c.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, inner := range row.Components {
			if in, ok := inner.(*discordgo.TextInput); ok {
				fields[in.CustomID] = in.Value
			}
		}
	}
	return fields
}
