package discord

import (
	"context"
	"errors"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/discord/commands"
)

const (
	commandTimeout      = 30 * time.Second
	autocompleteTimeout = 2500 * time.Millisecond
)

// interactionAPI is the part of *discordgo.Session used to answer interactions.
type interactionAPI interface {
	InteractionRespond(i *discordgo.Interaction, resp *discordgo.InteractionResponse, options ...discordgo.RequestOption) error
	InteractionResponseEdit(i *discordgo.Interaction, edit *discordgo.WebhookEdit, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// noMentions stops replies (which embed Home Assistant names) from pinging anyone.
func noMentions() *discordgo.MessageAllowedMentions {
	return &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
}

func interactionUserID(i *discordgo.Interaction) string {
	switch {
	case i.Member != nil && i.Member.User != nil:
		return i.Member.User.ID
	case i.User != nil:
		return i.User.ID
	default:
		return ""
	}
}

// serve runs handleInteraction and contains any panic so that one malformed
// interaction can never take the whole process down.
func (b *Bot) serve(ctx context.Context, api interactionAPI, i *discordgo.Interaction) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("interaction handler panicked", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	b.handleInteraction(ctx, api, i)
}

// handleInteraction is the single entry point for every interaction. The
// guild and user are authorized first, for every interaction type, so no
// handler can be reached by an unauthorized user.
func (b *Bot) handleInteraction(ctx context.Context, api interactionAPI, i *discordgo.Interaction) {
	actor, err := b.auth.Authorize(i.GuildID, interactionUserID(i))
	if err != nil {
		b.reject(api, i, err)
		return
	}
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		b.runCommand(ctx, api, i, actor)
	case discordgo.InteractionApplicationCommandAutocomplete:
		b.autocomplete(ctx, api, i, actor)
	case discordgo.InteractionMessageComponent:
		b.runComponent(ctx, api, i, actor)
	case discordgo.InteractionModalSubmit:
		b.runModalSubmit(ctx, api, i, actor)
	default:
		b.say(api, i, "That kind of interaction is not supported.")
	}
}

func (b *Bot) reject(api interactionAPI, i *discordgo.Interaction, cause error) {
	reason := "not_authorized"
	if errors.Is(cause, auth.ErrWrongGuild) {
		reason = "wrong_guild"
	}
	b.log.Warn("rejected unauthorized interaction", "reason", reason, "user_id", interactionUserID(i), "guild_id", i.GuildID, "type", i.Type.String())
	if i.Type == discordgo.InteractionApplicationCommandAutocomplete {
		b.respondChoices(api, i, nil)
		return
	}
	b.say(api, i, "You are not authorized to use NeptHass.")
}

func (b *Bot) runCommand(ctx context.Context, api interactionAPI, i *discordgo.Interaction, actor auth.Actor) {
	if !b.limiter.Allow(actor.UserID) {
		b.say(api, i, "You are sending commands too quickly. Wait a few seconds and try again.")
		return
	}
	// Acknowledge within Discord's 3 second limit; Home Assistant may be slower.
	ack := &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	}
	if err := api.InteractionRespond(i, ack); err != nil {
		b.log.Warn("could not acknowledge interaction", "error", err)
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	reply := b.reg.Run(runCtx, newRequest(actor, i.GuildID, i.ApplicationCommandData()))

	b.deliver(api, i, reply)
}

// deliver edits the deferred response (or the component's message) with a reply.
func (b *Bot) deliver(api interactionAPI, i *discordgo.Interaction, reply commands.Reply) {
	embeds, components := reply.Embeds, reply.Components
	if embeds == nil {
		embeds = []*discordgo.MessageEmbed{}
	}
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	edit := &discordgo.WebhookEdit{Content: &reply.Content, Embeds: &embeds, Components: &components, AllowedMentions: noMentions()}
	if _, err := api.InteractionResponseEdit(i, edit); err != nil {
		b.log.Warn("could not deliver reply", "error", err)
	}
}

func (b *Bot) autocomplete(ctx context.Context, api interactionAPI, i *discordgo.Interaction, actor auth.Actor) {
	ctx, cancel := context.WithTimeout(ctx, autocompleteTimeout)
	defer cancel()
	b.respondChoices(api, i, b.reg.Complete(ctx, newRequest(actor, i.GuildID, i.ApplicationCommandData())))
}

func (b *Bot) respondChoices(api interactionAPI, i *discordgo.Interaction, choices []*discordgo.ApplicationCommandOptionChoice) {
	if choices == nil {
		choices = []*discordgo.ApplicationCommandOptionChoice{}
	}
	resp := &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	}
	if err := api.InteractionRespond(i, resp); err != nil {
		b.log.Warn("could not deliver autocomplete choices", "error", err)
	}
}

// say sends an immediate ephemeral message.
func (b *Bot) say(api interactionAPI, i *discordgo.Interaction, content string) {
	resp := &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: content, Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: noMentions()},
	}
	if err := api.InteractionRespond(i, resp); err != nil {
		b.log.Warn("could not respond to interaction", "error", err)
	}
}

// newRequest flattens Discord's option tree into a commands.Request.
func newRequest(actor auth.Actor, guildID string, data discordgo.ApplicationCommandInteractionData) commands.Request {
	req := commands.Request{Actor: actor, GuildID: guildID, Command: data.Name, Args: make(map[string]string)}
	opts := data.Options
	if len(opts) == 1 && opts[0].Type == discordgo.ApplicationCommandOptionSubCommand {
		req.Subcommand = opts[0].Name
		opts = opts[0].Options
	}
	for _, o := range opts {
		switch o.Type {
		case discordgo.ApplicationCommandOptionString:
			req.Args[o.Name] = o.StringValue()
		case discordgo.ApplicationCommandOptionInteger:
			req.Args[o.Name] = strconv.FormatInt(o.IntValue(), 10)
		default:
			// No NeptHass command declares other option types.
		}
		if o.Focused {
			req.Focused = o.Name
		}
	}
	return req
}
