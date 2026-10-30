package discord

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/monitor"
	"github.com/nepthass/nepthass/internal/notify"
)

var bg = context.Background()

func door(name string) notify.Message {
	return notify.Message{Kind: notify.MsgAlert, Category: monitor.CategoryDoor, Name: name, Trigger: notify.KindState,
		Meaning: monitor.MeaningOpen, ObservedAt: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
}

func TestNotifier_sendsToTheConfiguredChannelWithEmptyAllowedMentions(t *testing.T) {
	f := newNFix(t, true)
	if err := f.n.Send(bg, door("@everyone <@123> *x*")); err != nil {
		t.Fatal(err)
	}
	posts := f.stub.postsSnapshot()
	if len(posts) != 1 || posts[0].Channel != stubChannel {
		t.Fatalf("one message to the configured channel: %+v", posts)
	}
	content, _ := posts[0].Body["content"].(string)
	if !strings.Contains(content, "is now open") || !strings.Contains(content, "\\@everyone") || strings.Contains(content, "binary_sensor") {
		t.Errorf("content must carry the escaped name and no entity ID: %q", content)
	}
	am, ok := posts[0].Body["allowed_mentions"].(map[string]any)
	if !ok {
		t.Fatalf("allowed_mentions must be sent explicitly: %+v", posts[0].Body)
	}
	if parse, isList := am["parse"].([]any); !isList || len(parse) != 0 || am["users"] != nil || am["roles"] != nil || am["replied_user"] == true {
		t.Errorf("no users, roles or everyone may be mentioned: %+v", am)
	}
	if f.n.Status().LastSuccess.IsZero() || f.n.Status().LastFailure != "" {
		t.Errorf("status = %+v", f.n.Status())
	}
}

func TestNotifier_mapsDiscordFailuresToTheDispatchersCategories_withOneAttemptEach(t *testing.T) {
	cases := []struct {
		name     string
		reply    stubReply
		kind     notify.FailureKind
		category string
	}{
		{"rate limited", stubReply{429, `{"message":"You are being rate limited.","retry_after":3,"global":false}`}, notify.FailureRateLimited, "rate_limited"},
		{"server error", stubReply{500, `{"message":"oops"}`}, notify.FailureRetryable, "server_error"},
		{"bad gateway (discordgo would retry this itself)", stubReply{502, `{"message":"bad gateway"}`}, notify.FailureRetryable, "server_error"},
		{"missing permissions", stubReply{403, `{"message":"Missing Permissions","code":50013}`}, notify.FailurePermanent, "missing_permissions"},
		{"missing access", stubReply{403, `{"message":"Missing Access","code":50001}`}, notify.FailurePermanent, "missing_permissions"},
		{"channel deleted", stubReply{404, `{"message":"Unknown Channel","code":10003}`}, notify.FailurePermanent, "channel_missing"},
		{"bad token", stubReply{401, `{"message":"401: Unauthorized","code":0}`}, notify.FailurePermanent, "unauthorized"},
		{"invalid body", stubReply{400, `{"message":"Invalid Form Body","code":50035}`}, notify.FailurePermanent, "bad_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newNFix(t, true)
			rp := tc.reply
			f.stub.reply = func(int) *stubReply { return &rp }
			err := f.n.Send(bg, door("Front door"))
			var se *notify.SendError
			if !errors.As(err, &se) || se.Kind != tc.kind {
				t.Fatalf("error = %v, want kind %d", err, tc.kind)
			}
			if tc.kind == notify.FailureRateLimited && se.RetryAfter <= 0 {
				t.Errorf("a rate limit must carry its RetryAfter: %+v", se)
			}
			if n := len(f.stub.postsSnapshot()); n != 1 {
				t.Errorf("exactly one request per attempt (no hidden discordgo retry), got %d", n)
			}
			if got := f.n.Status().LastFailure; got != tc.category {
				t.Errorf("status category = %q, want %q", got, tc.category)
			}
		})
	}

	f := newNFix(t, true)
	f.stub.srv.Close() // nothing answers: an ambiguous network failure
	var se *notify.SendError
	if err := f.n.Send(bg, door("x")); !errors.As(err, &se) || se.Kind != notify.FailureRetryable {
		t.Errorf("a network failure must be retried: %v", err)
	}
	g := newNFix(t, true)
	cancelled, cancel := context.WithCancel(bg)
	cancel()
	if err := g.n.Send(cancelled, door("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled context is returned as such so shutdown is not mistaken for a failure: %v", err)
	}
}

func TestNotifier_withoutAChannelOrASession(t *testing.T) {
	f := newNFix(t, false)
	var se *notify.SendError
	if err := f.n.Send(bg, door("x")); !errors.As(err, &se) || se.Kind != notify.FailurePermanent || len(f.stub.postsSnapshot()) != 0 {
		t.Errorf("no channel configured is permanent and sends nothing: %v", err)
	}
	g := newNFix(t, true)
	g.n.Bind(nil)
	if err := g.n.Send(bg, door("x")); !errors.As(err, &se) || se.Kind != notify.FailureRetryable {
		t.Errorf("an unbound session is retryable: %v", err)
	}
}

func TestNotifier_validatesTheChannel(t *testing.T) {
	text := int(discordgo.ChannelTypeGuildText)
	cases := []struct {
		name    string
		channel stubChannelInfo
		id      string
		want    string // substring of the user-facing reason; empty means accepted
	}{
		{"valid text channel", stubChannelInfo{stubGuild, text, "alerts", 0, nil}, stubChannel, ""},
		{"unknown channel", stubChannelInfo{}, "999000000000000009", "does not exist"},
		{"another server", stubChannelInfo{"400000000000000004", text, "elsewhere", 0, nil}, stubChannel, "not in this server"},
		{"direct message", stubChannelInfo{"", int(discordgo.ChannelTypeDM), "dm", 0, nil}, stubChannel, "not in this server"},
		{"voice channel", stubChannelInfo{stubGuild, int(discordgo.ChannelTypeGuildVoice), "voice", 0, nil}, stubChannel, "normal text channel"},
		{"forum channel", stubChannelInfo{stubGuild, int(discordgo.ChannelTypeGuildForum), "forum", 0, nil}, stubChannel, "normal text channel"},
		{"announcement channel", stubChannelInfo{stubGuild, int(discordgo.ChannelTypeGuildNews), "news", 0, nil}, stubChannel, "normal text channel"},
		{"thread", stubChannelInfo{stubGuild, int(discordgo.ChannelTypeGuildPublicThread), "thread", 0, nil}, stubChannel, "normal text channel"},
		{"bot cannot send", stubChannelInfo{stubGuild, text, "alerts", discordgo.PermissionSendMessages, nil}, stubChannel, "Send Messages"},
		{"bot cannot embed", stubChannelInfo{stubGuild, text, "alerts", discordgo.PermissionEmbedLinks, nil}, stubChannel, "Embed Links"},
		{"bot cannot see it", stubChannelInfo{stubGuild, text, "alerts", discordgo.PermissionViewChannel, nil}, stubChannel, "View Channel"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newNFix(t, false)
			if tc.channel.Name != "" {
				f.stub.channels[tc.id] = tc.channel
			}
			name, err := f.n.ValidateChannel(bg, tc.id)
			if tc.want == "" {
				if err != nil || name != "alerts" {
					t.Fatalf("accepted channel: %q %v", name, err)
				}
				return
			}
			var ue *commands.UserError
			if !errors.As(err, &ue) || !strings.Contains(ue.Msg, tc.want) {
				t.Errorf("error = %v, want a UserError containing %q", err, tc.want)
			}
		})
	}
}

func TestNotifier_failuresAreVisibleInStatus_andSuccessClearsThem(t *testing.T) {
	f := newNFix(t, true)
	f.stub.reply = func(n int) *stubReply {
		if n == 1 {
			return &stubReply{403, `{"message":"Missing Permissions","code":50013}`}
		}
		return nil
	}
	_ = f.n.Send(bg, door("x"))
	if st := f.n.Status(); st.LastFailure != "missing_permissions" || st.LastFailureAt.IsZero() {
		t.Fatalf("permission loss must be visible: %+v", st)
	}
	if err := f.n.Send(bg, door("x")); err != nil {
		t.Fatal(err)
	}
	if st := f.n.Status(); st.LastFailure != "" || st.LastSuccess.IsZero() {
		t.Errorf("a later success clears the failure: %+v", st)
	}
}

// Sending uses REST only. Source reading shows Session.Close touches just the
// gateway, and this proves a closed session still sends. It cannot prove how a
// session behaves after closing a LIVE gateway connection (no real gateway here).
func TestNotifier_restSendingWorksAfterTheSessionIsClosed(t *testing.T) {
	f := newNFix(t, true)
	if err := f.sess.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.n.Send(bg, door("Front door")); err != nil || len(f.stub.postsSnapshot()) != 1 {
		t.Errorf("REST must keep working after Close: %v", err)
	}
}

func TestFormatMessage_everyKindWithoutEntityIDsAndWithTheRightCaveats(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	thr := 28.0
	cases := map[string]struct {
		m    notify.Message
		want []string
		not  []string
	}{
		"state": {door("Front door"), []string{"**Front door** is now open", "<t:"}, []string{"Detected after", "back to normal", "unknown"}},
		"late":  {func() notify.Message { m := door("Front door"); m.Late = true; return m }(), []string{"reconnected to Home Assistant", "may have happened earlier"}, nil},
		"uncertain leak": {func() notify.Message {
			m := door("Basement")
			m.Category, m.Meaning, m.Uncertain = monitor.CategoryLeak, monitor.MeaningWet, true
			return m
		}(), []string{"is wet", "unknown or unavailable before", "please check it"}, nil},
		"cleared and muted": {func() notify.Message { m := door("D"); m.Cleared, m.Muted = true, 2; return m }(), []string{"already back to normal", "2 more alert(s)"}, nil},
		"numeric": {notify.Message{Kind: notify.MsgAlert, Category: monitor.CategoryTemperature, Name: "Living room", Trigger: notify.KindAbove,
			Threshold: &thr, Value: 29.5, HasValue: true, Unit: "°C", ObservedAt: at}, []string{"rose above 28 °C", "(now 29.5 °C)"}, nil},
		"down": {notify.Message{Kind: notify.MsgMonitoringDown, DownSince: at, DownFor: 5 * time.Minute, Failure: monitor.FailureNetwork},
			[]string{"down for 5 minutes", "paused until it recovers", "could not be reached"}, nil},
		"recovered": {notify.Message{Kind: notify.MsgMonitoringRecovered, DownFor: 7 * time.Minute}, []string{"is back after 7 minutes"}, nil},
		"digest": {notify.Message{Kind: notify.MsgDigest, MoreLines: 3, Lines: []notify.DigestLine{{Name: "Front door", Trigger: notify.KindState, Meaning: monitor.MeaningOpen, ChangedAt: at}}},
			[]string{"Current state", "**Front door**: is now open", "and 3 more", "what happened in between is not known"}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := FormatMessage(tc.m)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in %q", w, got)
				}
			}
			for _, n := range append(tc.not, "binary_sensor", "sensor.", "entity") {
				if strings.Contains(got, n) {
					t.Errorf("unexpected %q in %q", n, got)
				}
			}
			if len(got) > 2000 {
				t.Errorf("message longer than Discord allows: %d", len(got))
			}
		})
	}
}

const (
	pView  = discordgo.PermissionViewChannel
	pSend  = discordgo.PermissionSendMessages
	pEmbed = discordgo.PermissionEmbedLinks
	pAll   = pView | pSend | pEmbed
	roleR1 = "700000000000000007"
)

// The permission maths belongs to discordgo; these tests pin that NeptHass feeds
// it real roles and overwrites (through the REST fallback) and reads the result.
func TestNotifier_validatesRoleAndOverwriteCombinations(t *testing.T) {
	text := int(discordgo.ChannelTypeGuildText)
	cases := []struct {
		name      string
		everyone  int64
		roles     map[string]int64
		member    []string
		owner     string
		overwrite []stubOverwrite
		want      string // empty: accepted
	}{
		{"a role grants what @everyone lacks", pView, map[string]int64{roleR1: pSend | pEmbed}, []string{roleR1}, "", nil, ""},
		{"@everyone is denied send but the bot's role overwrite allows it", pAll, map[string]int64{roleR1: 0}, []string{roleR1}, "",
			[]stubOverwrite{{stubGuild, 0, 0, pSend}, {roleR1, 0, pSend, 0}}, ""},
		{"a member overwrite denies embed links", pAll, nil, nil, "", []stubOverwrite{{stubBot, 1, 0, pEmbed}}, "Embed Links"},
		{"a role overwrite denies send", pAll, map[string]int64{roleR1: 0}, []string{roleR1}, "", []stubOverwrite{{roleR1, 0, 0, pSend}}, "Send Messages"},
		{"an administrator role bypasses every overwrite", 0, map[string]int64{roleR1: discordgo.PermissionAdministrator}, []string{roleR1}, "",
			[]stubOverwrite{{stubGuild, 0, 0, pAll}}, ""},
		{"the guild owner has everything", 0, nil, nil, stubBot, nil, ""},
		{"nothing grants send or embed", pView, nil, nil, "", nil, "Send Messages, Embed Links"},
		{"an overwrite for a role the bot does not hold changes nothing", pAll, map[string]int64{roleR1: 0}, nil, "", []stubOverwrite{{roleR1, 0, 0, pSend}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newNFix(t, false)
			f.stub.everyone, f.stub.roles, f.stub.member, f.stub.owner = tc.everyone, tc.roles, tc.member, tc.owner
			f.stub.channels[stubChannel] = stubChannelInfo{GuildID: stubGuild, Type: text, Name: "alerts", Overwrites: tc.overwrite}
			_, err := f.n.ValidateChannel(bg, stubChannel)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("must be accepted: %v", err)
				}
				return
			}
			var ue *commands.UserError
			if !errors.As(err, &ue) || ue.Reason != "missing_permissions" || !strings.Contains(ue.Msg, tc.want) {
				t.Errorf("error = %v, want missing %q", err, tc.want)
			}
		})
	}
}
