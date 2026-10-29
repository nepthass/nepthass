package discord

import (
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/auth"
)

func reqOpt(name string, t discordgo.ApplicationCommandOptionType, v any) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: t, Value: v}
}

func reqSub(name string, opts ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionSubCommand, Options: opts}
}

func parseReq(name string, opts ...*discordgo.ApplicationCommandInteractionDataOption) (map[string]string, []string, string) {
	r := newRequest(auth.Actor{UserID: "1", Level: auth.LevelAdmin}, "g", discordgo.ApplicationCommandInteractionData{Name: name, Options: opts})
	return r.Args, r.BadOptions, r.Subcommand
}

func TestRequest_parsesChannelAndNumberOptions(t *testing.T) {
	args, bad, subcmd := parseReq("notifications", reqSub("channel", reqOpt("channel", discordgo.ApplicationCommandOptionChannel, "200000000000000002")))
	if subcmd != "channel" || args["channel"] != "200000000000000002" || len(bad) != 0 {
		t.Errorf("channel: %v %v %q", args, bad, subcmd)
	}
	args, bad, _ = parseReq("notifications", reqSub("add", reqOpt("entity", discordgo.ApplicationCommandOptionString, "sensor.a"),
		reqOpt("above", discordgo.ApplicationCommandOptionNumber, 28.5), reqOpt("below", discordgo.ApplicationCommandOptionNumber, -3.0),
		reqOpt("cooldown", discordgo.ApplicationCommandOptionInteger, 15.0)))
	if args["above"] != "28.5" || args["below"] != "-3" || args["cooldown"] != "15" || args["entity"] != "sensor.a" || len(bad) != 0 {
		t.Errorf("numbers: %v %v", args, bad)
	}
	args, _, _ = parseReq("notifications", reqSub("add", reqOpt("above", discordgo.ApplicationCommandOptionNumber, 1e-7), reqOpt("below", discordgo.ApplicationCommandOptionNumber, 1000000.0)))
	if args["above"] != "0.0000001" || args["below"] != "1000000" {
		t.Errorf("no exponent notation reaches the command: %v", args)
	}
}

// While an option is being autocompleted Discord sends its PARTIAL TEXT as a
// string, even for an Integer option; IntValue would panic on it.
func TestRequest_aFocusedIntegerOrNumberCarriesItsPartialText_withoutPanicking(t *testing.T) {
	o := reqOpt("rule", discordgo.ApplicationCommandOptionInteger, "4")
	o.Focused = true
	r := newRequest(auth.Actor{}, "g", discordgo.ApplicationCommandInteractionData{Name: "notifications", Options: []*discordgo.ApplicationCommandInteractionDataOption{reqSub("pause", o)}})
	if r.Args["rule"] != "4" || r.Focused != "rule" || len(r.BadOptions) != 0 {
		t.Errorf("request = %+v", r)
	}
	n := reqOpt("above", discordgo.ApplicationCommandOptionNumber, "2")
	if args, bad, _ := parseReq("notifications", reqSub("add", n)); args["above"] != "2" || len(bad) != 0 {
		t.Errorf("%v %v", args, bad)
	}
}

func TestRequest_unsupportedOptionTypesAreFlaggedNeverSilentlyIgnored(t *testing.T) {
	for name, typ := range map[string]discordgo.ApplicationCommandOptionType{
		"flag": discordgo.ApplicationCommandOptionBoolean, "who": discordgo.ApplicationCommandOptionUser, "role": discordgo.ApplicationCommandOptionRole,
		"both": discordgo.ApplicationCommandOptionMentionable, "file": discordgo.ApplicationCommandOptionAttachment,
	} {
		_, bad, _ := parseReq("x", reqOpt(name, typ, "v"))
		if len(bad) != 1 || bad[0] != name {
			t.Errorf("%s must be flagged: %v", name, bad)
		}
	}
	_, bad, _ := parseReq("x", reqOpt("n", discordgo.ApplicationCommandOptionInteger, true))
	if len(bad) != 1 {
		t.Errorf("a value of the wrong kind is flagged too: %v", bad)
	}
}

func TestRequest_existingCommandShapesAreUnchanged(t *testing.T) {
	args, bad, subcmd := parseReq("permissions", reqSub("allow", reqOpt("entity", discordgo.ApplicationCommandOptionString, "light.kitchen"), reqOpt("access", discordgo.ApplicationCommandOptionString, "control")))
	if subcmd != "allow" || args["entity"] != "light.kitchen" || args["access"] != "control" || len(bad) != 0 {
		t.Errorf("/permissions allow: %v %v %q", args, bad, subcmd)
	}
	args, bad, _ = parseReq("audit", reqOpt("count", discordgo.ApplicationCommandOptionInteger, 10.0))
	if args["count"] != "10" || len(bad) != 0 {
		t.Errorf("/audit count: %v %v", args, bad)
	}
	args, bad, _ = parseReq("control", reqOpt("entity", discordgo.ApplicationCommandOptionString, "light.a"), reqOpt("action", discordgo.ApplicationCommandOptionString, "on"))
	if args["entity"] != "light.a" || args["action"] != "on" || len(bad) != 0 {
		t.Errorf("/control: %v %v", args, bad)
	}
}
