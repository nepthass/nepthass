package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/notify"
)

// Notifier delivers notifications to the configured guild text channel over
// Discord's REST API. It implements notify.Sender and validates channels.
//
// Retry layers: discordgo is told not to retry (no rate-limit retry, no 502
// retry), so the only retry logic is the notify dispatcher's: it owns the TTL,
// the backoff and the circuit breaker. A 429 is reported with its RetryAfter.
// Sending uses REST only, so it does not depend on the gateway connection.
type Notifier struct {
	guildID string
	store   *notify.Store
	log     *slog.Logger
	session atomic.Pointer[discordgo.Session]

	mu         sync.Mutex
	lastOK     time.Time
	lastFail   string
	lastFailAt time.Time
}

// NewNotifier returns a Notifier; Bind supplies the session before any send.
func NewNotifier(guildID string, store *notify.Store, log *slog.Logger) *Notifier {
	return &Notifier{guildID: guildID, store: store, log: log}
}

// Bind sets the Discord session (created by the Bot).
func (n *Notifier) Bind(s *discordgo.Session) { n.session.Store(s) }

// NotifierStatus is what /notifications status shows about delivery.
type NotifierStatus struct {
	LastSuccess   time.Time
	LastFailure   string // empty when the last attempt succeeded
	LastFailureAt time.Time
}

// Status returns the last delivery outcome.
func (n *Notifier) Status() NotifierStatus {
	n.mu.Lock()
	defer n.mu.Unlock()
	return NotifierStatus{LastSuccess: n.lastOK, LastFailure: n.lastFail, LastFailureAt: n.lastFailAt}
}

func (n *Notifier) record(category string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if category == "" {
		n.lastOK, n.lastFail = time.Now(), ""
		return
	}
	n.lastFail, n.lastFailAt = category, time.Now()
}

// Send implements notify.Sender. The channel is read from the settings at send
// time, so a channel change applies immediately and a deleted or locked channel
// is noticed on the very next attempt.
func (n *Notifier) Send(ctx context.Context, m notify.Message) error {
	st, ok, err := n.store.GetSettings(ctx, n.guildID)
	if err != nil {
		return notify.Retryable(err)
	}
	if !ok {
		return notify.Permanent(errors.New("no alert channel is configured"))
	}
	return n.post(ctx, st.ChannelID, FormatMessage(m))
}

// SendTest sends a fixed test message to the configured channel.
func (n *Notifier) SendTest(ctx context.Context) error {
	st, ok, err := n.store.GetSettings(ctx, n.guildID)
	if err != nil {
		return err
	}
	if !ok {
		return notify.ErrNotConfigured
	}
	err = n.post(ctx, st.ChannelID, "NeptHass test message: if you can read this, alerts will arrive in this channel.")
	if err == nil {
		return nil
	}
	return &commands.UserError{Msg: n.lastProblemText()}
}

func (n *Notifier) lastProblemText() string {
	switch n.Status().LastFailure {
	case "missing_permissions":
		return "Discord refused the message: the bot lacks permission to view or send messages in that channel."
	case "channel_missing":
		return "That channel no longer exists or the bot cannot see it. Choose it again with /notifications channel."
	case "unauthorized":
		return "Discord rejected the bot's token."
	case "rate_limited":
		return "Discord is rate limiting the bot. Try again in a moment."
	}
	return "The test message could not be delivered (Discord did not accept it). Try again, or see /notifications status."
}

func (n *Notifier) post(ctx context.Context, channelID, content string) error {
	s := n.session.Load()
	if s == nil {
		return notify.Retryable(errors.New("the Discord session is not ready"))
	}
	_, err := s.ChannelMessageSendComplex(channelID,
		&discordgo.MessageSend{Content: content, AllowedMentions: noMentions()},
		discordgo.WithContext(ctx), discordgo.WithRetryOnRatelimit(false), discordgo.WithRestRetries(0))
	if err == nil {
		n.record("")
		return nil
	}
	mapped, category := classifySendError(err)
	if category != "" {
		n.record(category)
		n.log.Warn("notification delivery failed", "category", category)
	}
	return mapped
}

// classifySendError maps a discordgo error to the dispatcher's categories and a
// short category for status output. A cancelled context is returned unchanged.
func classifySendError(err error) (error, string) {
	var rl *discordgo.RateLimitError
	if errors.As(err, &rl) {
		return notify.RateLimited(err, rl.RetryAfter), "rate_limited"
	}
	var re *discordgo.RESTError
	if errors.As(err, &re) && re.Response != nil {
		code := 0
		if re.Message != nil {
			code = re.Message.Code
		}
		switch status := re.Response.StatusCode; {
		case status == http.StatusUnauthorized:
			return notify.Permanent(err), "unauthorized"
		case status == http.StatusForbidden || code == discordgo.ErrCodeMissingAccess || code == discordgo.ErrCodeMissingPermissions:
			return notify.Permanent(err), "missing_permissions"
		case status == http.StatusNotFound || code == discordgo.ErrCodeUnknownChannel:
			return notify.Permanent(err), "channel_missing"
		case status == http.StatusTooManyRequests:
			return notify.RateLimited(err, time.Second), "rate_limited"
		case status >= 500:
			return notify.Retryable(err), "server_error"
		case status >= 400:
			return notify.Permanent(err), "bad_request"
		}
	}
	if errors.Is(err, context.Canceled) {
		return err, ""
	}
	// With its retries disabled discordgo reports a 502 as a plain error, not a RESTError.
	if strings.Contains(err.Error(), "Exceeded Max retries HTTP") {
		return notify.Retryable(err), "server_error"
	}
	return notify.Retryable(err), "network" // timeouts and resets are ambiguous: retried
}

// ValidateChannel checks a channel for use as the alert channel and returns its
// name: it exists, is a text channel of THIS guild, and the bot can view it,
// send messages and embed links. Failures are *commands.UserError.
func (n *Notifier) ValidateChannel(ctx context.Context, channelID string) (string, error) {
	s := n.session.Load()
	if s == nil {
		return "", &commands.UserError{Msg: "The Discord connection is not ready yet. Try again in a moment.", Reason: "unavailable"}
	}
	opt := discordgo.WithContext(ctx)
	ch, err := s.Channel(channelID, opt)
	if err != nil {
		var re *discordgo.RESTError
		if errors.As(err, &re) && re.Response != nil {
			switch re.Response.StatusCode {
			case http.StatusNotFound:
				return "", &commands.UserError{Msg: "That channel does not exist.", Reason: "not_found"}
			case http.StatusForbidden:
				return "", &commands.UserError{Msg: "The bot cannot see that channel. Give it access first.", Reason: "no_access"}
			}
		}
		return "", &commands.UserError{Msg: "Discord did not answer when checking the channel. Try again.", Reason: "unavailable"}
	}
	switch {
	case ch.GuildID != n.guildID:
		return "", &commands.UserError{Msg: "That channel is not in this server.", Reason: "other_guild"}
	case ch.Type != discordgo.ChannelTypeGuildText:
		return "", &commands.UserError{Msg: "Choose a normal text channel, not a voice, forum, announcement, thread or direct-message channel.", Reason: "not_text"}
	}
	me, err := s.User("@me", opt)
	if err != nil {
		return "", &commands.UserError{Msg: "Discord did not answer when checking the bot's permissions. Try again.", Reason: "unavailable"}
	}
	perms, err := s.UserChannelPermissions(me.ID, channelID, opt)
	if err != nil {
		return "", &commands.UserError{Msg: "Discord did not answer when checking the bot's permissions. Try again.", Reason: "unavailable"}
	}
	var missing []string
	for _, p := range []struct {
		bit  int64
		name string
	}{{discordgo.PermissionViewChannel, "View Channel"}, {discordgo.PermissionSendMessages, "Send Messages"}, {discordgo.PermissionEmbedLinks, "Embed Links"}} {
		if perms&p.bit == 0 {
			missing = append(missing, p.name)
		}
	}
	if len(missing) > 0 {
		return "", &commands.UserError{Msg: fmt.Sprintf("The bot is missing these permissions in that channel: %s.", joinNames(missing)), Reason: "missing_permissions"}
	}
	return ch.Name, nil
}

func joinNames(n []string) string {
	out := ""
	for i, s := range n {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
