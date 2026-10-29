package discord

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/notify"
	"github.com/nepthass/nepthass/internal/storage"
)

const (
	stubGuild   = "100000000000000001"
	stubChannel = "200000000000000002"
	stubBot     = "300000000000000003"
	stubWait    = 10 * time.Second
)

type stubChannelInfo struct {
	GuildID string
	Type    int
	Name    string
	Deny    int64 // permission bits denied to @everyone (and so to the bot) in this channel
}

type stubReply struct {
	Status int
	Body   string
}

type stubPost struct {
	Channel string
	Body    map[string]any
}

// stubDiscord stands in for Discord's REST API. The discordgo endpoint
// variables are redirected to it for the duration of a test, so tests must not
// run in parallel.
type stubDiscord struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	posts    []stubPost
	changed  chan struct{}
	channels map[string]stubChannelInfo
	everyone int64
	reply    func(n int) *stubReply // nil result: 200
	gate     chan struct{}          // when set, a POST blocks until it is closed
}

func useStub(t *testing.T) *stubDiscord {
	t.Helper()
	s := &stubDiscord{t: t, changed: make(chan struct{}), channels: map[string]stubChannelInfo{},
		everyone: discordgo.PermissionViewChannel | discordgo.PermissionSendMessages | discordgo.PermissionEmbedLinks}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	api, guilds, channels, users := discordgo.EndpointAPI, discordgo.EndpointGuilds, discordgo.EndpointChannels, discordgo.EndpointUsers
	discordgo.EndpointAPI = s.srv.URL + "/"
	discordgo.EndpointGuilds, discordgo.EndpointChannels, discordgo.EndpointUsers = discordgo.EndpointAPI+"guilds/", discordgo.EndpointAPI+"channels/", discordgo.EndpointAPI+"users/"
	t.Cleanup(func() {
		discordgo.EndpointAPI, discordgo.EndpointGuilds, discordgo.EndpointChannels, discordgo.EndpointUsers = api, guilds, channels, users
		s.srv.Close()
	})
	s.channels[stubChannel] = stubChannelInfo{GuildID: stubGuild, Type: int(discordgo.ChannelTypeGuildText), Name: "alerts"}
	return s
}

func (s *stubDiscord) handle(w http.ResponseWriter, r *http.Request) {
	p := strings.Trim(r.URL.Path, "/")
	w.Header().Set("Content-Type", "application/json")
	write := func(status int, body string) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
	switch {
	case r.Method == http.MethodGet && p == "users/@me":
		write(200, `{"id":"`+stubBot+`","username":"nepthass","bot":true}`)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "channels/") && !strings.HasSuffix(p, "/messages"):
		s.mu.Lock()
		ch, ok := s.channels[strings.TrimPrefix(p, "channels/")]
		s.mu.Unlock()
		if !ok {
			write(404, `{"message":"Unknown Channel","code":10003}`)
			return
		}
		guild := ""
		if ch.GuildID != "" {
			guild = `"guild_id":"` + ch.GuildID + `",`
		}
		write(200, `{"id":"`+strings.TrimPrefix(p, "channels/")+`",`+guild+`"type":`+strconv.Itoa(ch.Type)+`,"name":"`+ch.Name+`","permission_overwrites":[{"id":"`+stubGuild+`","type":0,"allow":"0","deny":"`+strconv.FormatInt(ch.Deny, 10)+`"}]}`)
	case r.Method == http.MethodGet && strings.Contains(p, "/members/"):
		write(200, `{"user":{"id":"`+stubBot+`"},"roles":[],"joined_at":"2020-01-01T00:00:00.000000+00:00"}`)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "guilds/"):
		write(200, `{"id":"`+stubGuild+`","owner_id":"1","roles":[{"id":"`+stubGuild+`","name":"@everyone","permissions":"`+strconv.FormatInt(s.everyone, 10)+`"}]}`)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/messages"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.posts = append(s.posts, stubPost{Channel: strings.TrimSuffix(strings.TrimPrefix(p, "channels/"), "/messages"), Body: body})
		n, gate, reply := len(s.posts), s.gate, s.reply
		close(s.changed)
		s.changed = make(chan struct{})
		s.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		if reply != nil {
			if rp := reply(n); rp != nil {
				if rp.Status == 429 {
					w.Header().Set("Retry-After", "1")
				}
				write(rp.Status, rp.Body)
				return
			}
		}
		write(200, `{"id":"1","channel_id":"`+stubChannel+`","content":"ok"}`)
	default:
		write(404, `{"message":"unexpected request `+r.Method+" "+p+`","code":0}`)
	}
}

func (s *stubDiscord) postsSnapshot() []stubPost {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubPost(nil), s.posts...)
}

func (s *stubDiscord) waitPosts(n int) []stubPost {
	s.t.Helper()
	deadline := time.After(stubWait)
	for {
		s.mu.Lock()
		got, ch := append([]stubPost(nil), s.posts...), s.changed
		s.mu.Unlock()
		if len(got) >= n {
			return got
		}
		select {
		case <-ch:
		case <-deadline:
			s.t.Fatalf("only %d of %d messages reached Discord", len(got), n)
		}
	}
}

type nfix struct {
	stub  *stubDiscord
	n     *Notifier
	store *notify.Store
	db    *sql.DB
	sess  *discordgo.Session
}

func newNFix(t *testing.T, channelConfigured bool) *nfix {
	t.Helper()
	st := useStub(t)
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := notify.NewStore(db)
	sess, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	n := NewNotifier(stubGuild, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n.Bind(sess)
	if channelConfigured {
		if err := store.UpsertChannel(context.Background(), stubGuild, stubChannel, "900000000000000001", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	return &nfix{stub: st, n: n, store: store, db: db, sess: sess}
}
