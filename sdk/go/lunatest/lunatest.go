// Package lunatest runs a Go plugin in-process against a fake Luna: the real plugin API
// server with a throwaway database, a fake Twitch and a fake clock. Permissions come from
// the plugin's plugin.toml, so a missing entry fails the test.
//
//	func TestWelcome(t *testing.T) {
//		h := lunatest.New(t, newBot(), ".", nil)
//		h.Emit("twitch.channel.follow", map[string]any{"user": "fox"})
//		if got := h.Said(); len(got) != 1 { ... }
//	}
package lunatest

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"luna/internal/bus"
	"luna/internal/config"
	"luna/internal/event"
	"luna/internal/guard"
	"luna/internal/perms"
	"luna/internal/plugin"
	"luna/internal/plugin/pb"
	"luna/internal/store"
	luna "luna/sdk/go"
)

// Harness is one fake Luna around one bot.
type Harness struct {
	tb     testing.TB
	bot    *luna.Bot
	m      *plugin.Manifest
	Store  *store.Store
	Twitch *FakeTwitch
	Clock  *Clock
	Events []event.Event // everything the plugin emitted

	mu        sync.Mutex
	msgs      []guard.Msg
	seen      int // messages already returned by Said
	cooldowns map[string]time.Time
	stream    int64 // the current stream's number, 0 while offline
	streams   int64
	n         int
	links     map[string]func(luna.Args) (any, error)
}

// Clock is fake time, starting at 2026-01-01 12:00 UTC.
type Clock struct {
	mu  sync.Mutex
	now time.Time
	h   *Harness
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves time forward, running every timer that comes due on the way.
func (c *Clock) Advance(d time.Duration) {
	end := c.Now().Add(d)
	for {
		wait := c.h.bot.TestRunDue()
		if wait < 0 || c.Now().Add(wait).After(end) {
			break
		}
		c.mu.Lock()
		c.now = c.now.Add(wait)
		c.mu.Unlock()
	}
	c.mu.Lock()
	c.now = end
	c.mu.Unlock()
	c.h.bot.TestRunDue()
}

// FakeTwitch records moderation and answers lookups from its fields.
type FakeTwitch struct {
	mu      sync.Mutex
	Actions []*pb.ModRequest
	Users   map[string]*luna.TwitchUser // by login
	Chan    map[string]*luna.Channel    // by id; "" is this channel
	Live    luna.Stream
	Follows map[string]time.Time // user id → followed at

	Categories []luna.Category
	Schedule   []luna.Segment
	Ads        luna.AdSchedule
	Streams    map[string]luna.LiveStream // by login: who's live
	Writes     []string                   // "set_channel {...}", "clip", "marker {...}", "raid {...}"
}

func (f *FakeTwitch) Mod(_ context.Context, r *pb.ModRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Actions = append(f.Actions, r)
	return nil
}

func (f *FakeTwitch) Lookup(_ context.Context, call, user string, args []byte) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var a struct {
		Query  string   `json:"query"`
		Logins []string `json:"logins"`
	}
	json.Unmarshal(args, &a)
	switch call {
	case "categories":
		var out []luna.Category
		for _, c := range f.Categories {
			if strings.Contains(strings.ToLower(c.Name), strings.ToLower(a.Query)) {
				out = append(out, c)
			}
		}
		return out, nil
	case "schedule":
		return f.Schedule, nil
	case "streams":
		var out []luna.LiveStream
		for _, l := range a.Logins {
			if s, ok := f.Streams[l]; ok {
				out = append(out, s)
			}
		}
		return out, nil
	case "ads":
		return f.Ads, nil
	case "set_channel", "clip", "marker", "snooze", "raid", "unraid":
		f.Writes = append(f.Writes, strings.TrimSpace(call+" "+string(args)))
		switch call {
		case "clip":
			return luna.Clip{ID: "clip1", URL: "https://clips.twitch.tv/clip1"}, nil
		case "marker":
			return luna.Marker{ID: "m1", PositionS: 600}, nil
		case "snooze":
			f.Ads.NextAdAt = f.Ads.NextAdAt.Add(5 * time.Minute)
			f.Ads.SnoozeCount--
			return f.Ads, nil
		}
		return nil, nil
	case "user":
		for _, u := range f.Users {
			if strings.EqualFold(u.Login, strings.TrimPrefix(user, "@")) || u.ID == user {
				return u, nil
			}
		}
	case "channel":
		if c := f.Chan[user]; c != nil {
			return c, nil
		}
	case "stream":
		return f.Live, nil
	case "follow":
		if at, ok := f.Follows[user]; ok {
			return map[string]time.Time{"followed_at": at}, nil
		}
	}
	return nil, nil
}

// Mods returns the moderation actions so far, as "timeout fox 60s" style strings.
func (f *FakeTwitch) Mods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.Actions {
		s := a.Action + " " + a.UserId + a.MessageId
		if a.Seconds > 0 {
			s += fmt.Sprintf(" %ds", a.Seconds)
		}
		if a.Action == "shield" {
			s = fmt.Sprintf("shield %v", a.On)
		}
		out = append(out, s)
	}
	return out
}

// New starts a fake Luna for bot, reading permissions from dir/plugin.toml, with config
// overrides like luna.toml's [plugins.<name>].
func New(tb testing.TB, bot *luna.Bot, dir string, cfg map[string]any) *Harness {
	tb.Helper()
	m, err := plugin.Load(dir)
	if err != nil {
		tb.Fatal(err)
	}
	if m.ID == "" {
		m.ID = "dev.test.plugin"
	}
	st, err := store.Open(filepath.Join(tb.TempDir(), "luna.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { st.Close() })
	h := &Harness{tb: tb, bot: bot, m: m, Store: st, cooldowns: map[string]time.Time{},
		Twitch: &FakeTwitch{Users: map[string]*luna.TwitchUser{}, Chan: map[string]*luna.Channel{}, Follows: map[string]time.Time{}}}
	h.Clock = &Clock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), h: h}
	st.Now = h.Clock.Now

	c := config.Default()
	c.Chat.RateLimit = 1 << 20 // tests send in bursts; the guard's limiter has its own tests
	g := guard.New(func() *config.Config { return c })
	g.Live = func(_ context.Context, m guard.Msg) error {
		h.mu.Lock()
		h.msgs = append(h.msgs, m)
		h.mu.Unlock()
		return nil
	}
	b := bus.New()
	host := &plugin.Host{Bus: b, Store: st, Guard: g, Twitch: h.Twitch, Publish: func(e event.Event) {
		h.mu.Lock()
		h.Events = append(h.Events, e)
		h.mu.Unlock()
	}}
	sock, err := os.MkdirTemp("", "lunatest") // t.TempDir paths can be too long for a socket
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { os.RemoveAll(sock) })
	ln, err := net.Listen("unix", filepath.Join(sock, "host.sock"))
	if err != nil {
		tb.Fatal(err)
	}
	for k, v := range cfg { // as luna.toml would hold them
		if n, ok := v.(int); ok {
			cfg[k] = int64(n)
		}
	}
	effective, err := m.EffectiveConfig(cfg)
	if err != nil {
		tb.Fatal(err)
	}
	go host.Serve(plugin.Plugin{ID: m.ID, Manifest: m, Config: effective, Log: testLog{tb}}, ln)
	tb.Cleanup(func() { ln.Close() })
	conn, err := grpc.NewClient("unix://"+filepath.Join(sock, "host.sock"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { conn.Close() })
	bot.CrashOnError = true
	h.links = map[string]func(luna.Args) (any, error){}
	bot.TestLinks(func(use string, args luna.Args) (any, error) {
		h.mu.Lock()
		fn := h.links[use]
		h.mu.Unlock()
		if fn == nil {
			return nil, fmt.Errorf("%w: %s (fake it with h.Link)", luna.ErrLinkUnavailable, use)
		}
		return fn(args)
	})
	if err := bot.TestConnect(pb.NewHostClient(conn), h.Clock.Now, m.ID, effective); err != nil {
		tb.Fatal(err)
	}
	return h
}

type testLog struct{ tb testing.TB }

func (l testLog) Write(p []byte) (int, error) {
	l.tb.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// user expands the "fox" shorthand into a user object; roles may include Luna ranks.
func user(login string, roles []string) map[string]any {
	rank := "viewer"
	for _, r := range []string{"broadcaster", "head-mod", "luna-mod", "moderator"} {
		if slices.Contains(roles, r) {
			rank = r
			break
		}
	}
	if rank == "head-mod" || rank == "luna-mod" {
		roles = append(roles, "moderator")
	}
	return map[string]any{"id": "id-" + login, "login": login, "name": login, "roles": roles, "rank": rank}
}

// Emit fires an event at the plugin; data["user"] = "fox" is shorthand for a viewer.
// Events are not simulated, so chat and moderation reach the fakes.
func (h *Harness) Emit(typ string, data map[string]any) *luna.Event {
	h.tb.Helper()
	if data == nil {
		data = map[string]any{}
	}
	if s, ok := data["user"].(string); ok {
		data["user"] = user(s, nil)
	}
	switch typ {
	case "twitch.stream.online":
		h.streams++ // numbered like the daemon's, so streaks carry across streams
		h.stream = h.streams
		h.Twitch.mu.Lock()
		h.Twitch.Live = luna.Stream{Live: true, StartedAt: h.Clock.Now()}
		h.Twitch.mu.Unlock()
	case "twitch.stream.offline":
		h.stream = 0
		h.Twitch.mu.Lock()
		h.Twitch.Live = luna.Stream{}
		h.Twitch.mu.Unlock()
	case "twitch.chat.message":
		u, _ := data["user"].(map[string]any)
		id, _ := u["id"].(string)
		login, _ := u["login"].(string)
		prev, err := h.Store.SeeChatter(id, login, h.Clock.Now(), h.stream)
		if err != nil {
			h.tb.Fatal(err)
		}
		data["first"], data["away_s"] = prev == nil, int64(0)
		if prev != nil {
			data["away_s"] = int64(h.Clock.Now().Sub(prev.LastSeen).Seconds())
		}
		if _, ok := data["message_id"]; !ok {
			h.n++
			data["message_id"] = fmt.Sprintf("msg-%d", h.n)
		}
	}
	raw, _ := json.Marshal(data)
	h.n++
	ev := &luna.Event{ID: fmt.Sprintf("ev-%d", h.n), Type: typ, Source: "test", TS: h.Clock.Now(), Data: raw}
	h.bot.TestDispatch(ev)
	return ev
}

// Chat types line in chat as login. Roles are Twitch badges ("sub", "vip", "moderator",
// "broadcaster") or Luna ranks ("luna-mod", "head-mod"). Like Luna, a !command is routed
// to the plugin only if the user's rank has its capability and it's off cooldown.
// It returns what the plugin said in response.
func (h *Harness) Chat(login, line string, roles ...string) []string {
	h.tb.Helper()
	h.Said()
	u := user(login, roles)
	ev := h.Emit("twitch.chat.message", map[string]any{"user": u, "text": line, "emotes": 0})
	words := strings.Fields(line)
	if len(words) > 0 && strings.HasPrefix(words[0], "!") {
		name := strings.ToLower(strings.TrimPrefix(words[0], "!"))
		for _, c := range h.bot.TestChatCommands() {
			if c.Name != name && !slices.Contains(c.Aliases, name) {
				continue
			}
			rank, _ := perms.ParseRank(u["rank"].(string))
			capability := c.Capability
			if capability == "" {
				capability = "chat.commands.use"
			}
			if !perms.DefaultPolicy().Can(u["id"].(string), rank, capability) || !h.offCooldown(c, u["id"].(string)) {
				break
			}
			args, _ := json.Marshal(map[string]any{"args": words[1:]})
			if _, err := h.bot.TestCommand("chat", c.Name, args, ev); err != nil {
				h.tb.Fatalf("!%s: %v", c.Name, err)
			}
		}
	}
	return h.Said()
}

func (h *Harness) offCooldown(c luna.ChatCommand, userID string) bool {
	now := h.Clock.Now()
	g, u := c.Name, c.Name+"\x00"+userID
	for key, d := range map[string]string{g: c.Cooldown, u: c.UserCooldown} {
		if dur, _ := time.ParseDuration(d); dur > 0 && now.Sub(h.cooldowns[key]) < dur {
			return false
		}
	}
	h.cooldowns[g], h.cooldowns[u] = now, now
	return true
}

// CLI runs `luna <plugin> name` with already-parsed args; data comes back as the CLI
// would render it (through JSON).
func (h *Harness) CLI(name string, args luna.Args) (any, error) {
	raw, _ := json.Marshal(args)
	out, err := h.bot.TestCommand("cli", name, raw, nil)
	if err != nil || out == nil {
		return out, err
	}
	if s, ok := out.(string); ok {
		return s, nil
	}
	b, _ := json.Marshal(out)
	var v any
	json.Unmarshal(b, &v)
	return v, nil
}

// Link fakes a provider: h.Link("luna.points:spend", func(a luna.Args) (any, error) {...}).
// A nil fn takes it down again, so calls fail with ErrLinkUnavailable.
func (h *Harness) Link(use string, fn func(luna.Args) (any, error)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.links[use] = fn
}

// Provide calls the plugin's own provider for link, as the plugin caller would.
func (h *Harness) Provide(link, caller string, args luna.Args) (any, error) {
	out, err := h.bot.TestProvide(link, caller, args)
	if err != nil || out == nil {
		return out, err
	}
	b, _ := json.Marshal(out)
	var v any
	json.Unmarshal(b, &v)
	return v, nil
}

// Said returns the chat messages sent since the last call.
func (h *Harness) Said() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, m := range h.msgs[h.seen:] {
		out = append(out, m.Text)
	}
	h.seen = len(h.msgs)
	return out
}

// Messages is every message sent, with its mode, reply target and whisper recipient.
func (h *Harness) Messages() []guard.Msg {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.msgs)
}
