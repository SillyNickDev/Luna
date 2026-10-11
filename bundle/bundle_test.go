package bundle

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"luna/internal/capsule"
	"luna/internal/plugin"
	luna "luna/sdk/go"
	"luna/sdk/go/lunatest"
)

func start(t *testing.T, cfg map[string]any) *lunatest.Harness {
	h := lunatest.New(t, New(), ".", cfg)
	h.Twitch.Users["fox"] = &luna.TwitchUser{ID: "id-fox", Login: "fox", Name: "Fox", CreatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	h.Twitch.Chan["id-fox"] = &luna.Channel{ID: "id-fox", Login: "fox", Name: "Fox", Game: "VRChat", Title: "chill"}
	h.Twitch.Chan[""] = &luna.Channel{Game: "Elden Ring", Title: "first playthrough"}
	return h
}

func said(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("said %q, want %q", got, want)
	}
}

// The code's command declarations are what the harness routes by; plugin.toml is what Luna
// routes by. They must agree.
func TestManifestMatchesCode(t *testing.T) {
	if _, err := capsule.LoadManifest("."); err != nil {
		t.Fatal(err)
	}
	m, err := plugin.Load(".")
	if err != nil {
		t.Fatal(err)
	}
	d := New().Describe()
	var code []map[string]any
	b, _ := json.Marshal(d["chat"])
	json.Unmarshal(b, &code)
	if len(code) != len(m.Chat.Commands) {
		t.Fatalf("code has %d chat commands, plugin.toml %d", len(code), len(m.Chat.Commands))
	}
	for _, c := range code {
		i := slices.IndexFunc(m.Chat.Commands, func(mc plugin.ChatCommand) bool { return mc.Name == c["name"] })
		if i < 0 {
			t.Fatalf("!%s isn't in plugin.toml", c["name"])
		}
		mc := m.Chat.Commands[i]
		capab, _ := c["capability"].(string)
		cd, _ := c["cooldown"].(string)
		ucd, _ := c["user_cooldown"].(string)
		var aliases []string
		for _, a := range c["aliases"].([]any) {
			aliases = append(aliases, a.(string))
		}
		pd := func(s string) time.Duration { d, _ := time.ParseDuration(s); return d }
		if capab != mc.Capability || pd(cd) != time.Duration(mc.Cooldown.Global) || pd(ucd) != time.Duration(mc.Cooldown.User) ||
			!slices.Equal(aliases, mc.Aliases) {
			t.Errorf("!%s: code %v, plugin.toml %+v", c["name"], c, mc)
		}
	}
	var cli []plugin.CLICommand
	b, _ = json.Marshal(d["cli"])
	json.Unmarshal(b, &cli)
	slices.SortFunc(m.CLI.Commands, func(a, b plugin.CLICommand) int { return strings.Compare(a.Name, b.Name) })
	if !reflect.DeepEqual(cli, m.CLI.Commands) {
		t.Errorf("CLI commands differ:\ncode %+v\ntoml %+v", cli, m.CLI.Commands)
	}
	var raw struct {
		Config map[string]map[string]any `toml:"config"`
	}
	toml.Decode(string(Manifest), &raw)
	cfg := d["config"].(map[string]map[string]any)
	for k, f := range raw.Config {
		if cfg[k] == nil || cfg[k]["type"] != f["type"] {
			t.Errorf("config %s: plugin.toml %v, Settings %v", k, f, cfg[k])
		}
	}
	if len(cfg) != len(raw.Config) {
		t.Errorf("Settings has %d fields, plugin.toml [config] %d", len(cfg), len(raw.Config))
	}
	var events []string
	for _, e := range d["events"].([]string) {
		if !m.AllowsEvents(e) {
			t.Errorf("handles %s without [permissions].events", e)
		}
		events = append(events, e)
	}
	if len(events) != len(m.Permissions.Events) {
		t.Errorf("asks for events it doesn't handle: %v vs %v", m.Permissions.Events, events)
	}
}

func TestCustomCommands(t *testing.T) {
	h := start(t, nil)
	said(t, h.Chat("fox", "!cmd add !discord Join us")) // viewers lack commands.edit: never routed
	said(t, h.Chat("owl", "!cmd add !discord Join us, {user}! Used {count} times.", "moderator"), "Saved !discord.")
	said(t, h.Chat("fox", "!discord"), "Join us, fox! Used 1 times.")
	said(t, h.Chat("fox", "!discord")) // 5 s cooldown
	h.Clock.Advance(6 * time.Second)
	said(t, h.Chat("owl", "!cmd alias !dc !discord", "moderator"), "!dc now runs !discord.")
	said(t, h.Chat("fox", "!dc"), "Join us, fox! Used 2 times.")
	said(t, h.Chat("owl", "!cmd add !so nope", "moderator"), "!so is built in.")
	said(t, h.Chat("owl", "!cmd set !discord mode announce", "moderator"), "Saved !discord.")
	h.Clock.Advance(6 * time.Second)
	h.Chat("fox", "!discord")
	if m := h.Messages(); m[len(m)-1].Mode != "announce" {
		t.Fatalf("mode announce: %+v", m[len(m)-1])
	}
	said(t, h.Chat("owl", "!cmd remove !discord", "moderator"), "Removed !discord.")
	h.Clock.Advance(6 * time.Second)
	said(t, h.Chat("fox", "!dc")) // its alias went with it
	if got := render("{pick a|a} {random 3-3} {nope}", nil); got != "a 3 {nope}" {
		t.Fatalf("render: %q", got)
	}
}

func TestCountersQuotesInfo(t *testing.T) {
	h := start(t, nil)
	h.Emit("twitch.channel.update", map[string]any{"game": "Elden Ring"})
	said(t, h.Chat("owl", "!counter add deaths pergame", "moderator"), "Counter !deaths ready, counted per game. Mods: !deaths +, !deaths -, !deaths set <n>")
	said(t, h.Chat("fox", "!deaths +"), "Deaths in Elden Ring: 0") // viewers can look, not count
	said(t, h.Chat("owl", "!deaths +", "moderator"), "Deaths in Elden Ring: 1")
	said(t, h.Chat("owl", "!deaths set 12", "moderator"), "Deaths in Elden Ring: 12")
	h.Emit("twitch.channel.update", map[string]any{"game": "Celeste"})
	said(t, h.Chat("owl", "!deaths", "moderator"), "Deaths in Celeste: 0")
	if _, err := h.CLI("undo", luna.Args{"id": 2}); err != nil { // put Elden Ring back to 1
		t.Fatal(err)
	}
	h.Emit("twitch.channel.update", map[string]any{"game": "Elden Ring"})
	said(t, h.Chat("fox", "!deaths"), "Deaths in Elden Ring: 1")

	said(t, h.Chat("owl", "!quote add I am not lost", "moderator"), "Added quote #1.")
	said(t, h.Chat("fox", "!quote 1"), "#1: I am not lost [Elden Ring] 2026-01-01")
	h.Clock.Advance(6 * time.Second)
	said(t, h.Chat("fox", "!quote search L0ST"), "#1: I am not lost [Elden Ring] 2026-01-01")

	h.Twitch.Follows["id-fox"] = h.Clock.Now().Add(-49 * time.Hour)
	said(t, h.Chat("fox", "!followage"), "fox has followed for 2 days 1 hour.")
	said(t, h.Chat("owl", "!accountage fox"), "Fox's account is 6 years 2 days old.")
	said(t, h.Chat("fox", "!uptime"), "The stream is offline.")
	h.Emit("twitch.stream.online", nil)
	h.Clock.Advance(90 * time.Minute)
	said(t, h.Chat("fox", "!uptime"), "Live for 1 hour 30 minutes.")
	said(t, h.Chat("fox", "!lurk"), "fox is lurking. Enjoy the stream!")
	h.Clock.Advance(9 * time.Minute) // under the 10-minute activity window, so it counts as watching
	said(t, h.Chat("fox", "!unlurk"), "Welcome back, fox! You were away 9 minutes.")
	said(t, h.Chat("fox", "!watchtime"), "fox has watched for 9 minutes over 1 stream.")
	said(t, h.Chat("fox", "!birthday 13-40"), "Usage: !birthday MM-DD (just the day, never the year), or !birthday clear")
}

func TestTimers(t *testing.T) {
	h := start(t, map[string]any{"timer_min_messages": 2})
	said(t, h.Chat("owl", "!timer add hydrate 5 Drink water 💧", "moderator"), "Timer hydrate: every 5 min, 1 message.")
	said(t, h.Chat("owl", "!timer msg hydrate Stretch!", "moderator"), "Timer hydrate: every 5 min, 2 messages.")
	h.Clock.Advance(time.Minute) // first tick starts its clock
	h.Chat("fox", "hi")
	h.Chat("fox", "hello")
	h.Clock.Advance(10 * time.Minute)
	said(t, h.Said()) // offline: quiet
	h.Emit("twitch.stream.online", nil)
	h.Chat("fox", "hi")
	h.Chat("fox", "hello")
	h.Clock.Advance(time.Minute)
	said(t, h.Said(), "Drink water 💧")
	h.Clock.Advance(10 * time.Minute)
	said(t, h.Said()) // chat went quiet: no timer spam into an empty room
	h.Chat("fox", "a")
	h.Chat("fox", "b")
	h.Clock.Advance(time.Minute)
	said(t, h.Said(), "Stretch!")
}

func TestFilters(t *testing.T) {
	h := start(t, map[string]any{"mod_exempt": "vip"})
	h.Chat("spam", "buy followers at cheap-views.com now")
	if got := h.Twitch.Mods(); !slices.Equal(got, []string{"delete msg-1", "warn id-spam"}) {
		t.Fatalf("first strike: %q", got)
	}
	h.Chat("spam", "fr33 f0ll0wers at cheap-views.com")
	h.Chat("vip", "my clip https://example.com/x", "vip") // exempt
	h.Chat("owl", "see https://example.com", "moderator") // mods are never filtered
	h.Chat("fox", "clip: https://clips.twitch.tv/Abc")    // allowlisted
	said(t, h.Chat("owl", "!permit fox", "moderator"), "fox may post one link in the next 60s.")
	h.Chat("fox", "here: example.com") // permitted once
	if got := h.Twitch.Mods(); !slices.Equal(got[2:], []string{"timeout id-spam 60s"}) {
		t.Fatalf("second strike, others untouched: %q", got)
	}
	if _, err := h.CLI("phrase-add", luna.Args{"pattern": `\blost\b`}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.CLI("phrase-add", luna.Args{"pattern": `(a`}); err == nil {
		t.Fatal("a broken pattern was accepted")
	}
	h.Chat("cat", "you are L0ѕT") // l33t and a Cyrillic ѕ
	h.Chat("dog", "THIS IS ALL CAPS SHOUTING")
	h.Chat("eel", "same thing")
	h.Chat("eel", "same   thiiiing")
	h.Chat("eel", "SAME thing")
	got := strings.Join(h.Twitch.Mods()[3:], " ")
	if !strings.Contains(got, "warn id-cat") || !strings.Contains(got, "warn id-dog") || strings.Count(got, "warn id-eel") != 1 {
		t.Fatalf("phrase, caps, repeat (third time only): %q", got)
	}
	tl, _ := h.CLI("timeline", nil)
	if rows := tl.([]any); len(rows) != 5 || rows[3].(map[string]any)["action"] != "timeout" {
		t.Fatalf("timeline: %v", tl)
	}
}

func TestNukeShieldHistory(t *testing.T) {
	h := start(t, map[string]any{"mod_shield_follows": 3})
	h.Chat("a1", "raid spam text")
	h.Chat("a2", "RAID SPAM text!")
	h.Chat("owl", "raid spam text", "moderator")
	h.Chat("fox", "hello")
	said(t, h.Chat("owl", "!nuke raid spam", "moderator"), "Nuked 2 chatters (#1; !nuke undo reverses it).")
	said(t, h.Chat("owl", "!nuke undo", "moderator"), "undid #1 (nuke)")
	if got := h.Twitch.Mods(); !slices.Equal(got, []string{"timeout id-a1 600s", "timeout id-a2 600s", "unban id-a1", "unban id-a2"}) {
		t.Fatalf("nuke: %q", got)
	}
	for _, f := range []string{"b1", "b2", "b3"} {
		h.Emit("twitch.channel.follow", map[string]any{"user": f})
	}
	if got := h.Twitch.Mods(); got[len(got)-1] != "shield true" {
		t.Fatalf("follow wave didn't raise the shield: %q", got)
	}
	said(t, h.Said(), "🛡️ Shield Mode is on: 3 follows in 30s. Mods: !shield off to end it.")
	h.Clock.Advance(11 * time.Minute)
	said(t, h.Said(), "🛡️ Shield Mode is off.")
	said(t, h.Chat("owl", "!note fox nice person", "moderator"), "Noted for fox.")
	got := h.Chat("owl", "!mod history fox", "moderator")
	if len(got) != 1 || !strings.Contains(got[0], "first seen 2026-01-01") || !strings.Contains(got[0], "note (owl") {
		t.Fatalf("history: %q", got)
	}
	said(t, h.Chat("owl", "!so ghost", "moderator"), "No Twitch user ghost.")
}

func TestEventsAndShoutouts(t *testing.T) {
	h := start(t, nil)
	h.Emit("twitch.channel.follow", map[string]any{"user": "ana"})
	h.Clock.Advance(time.Minute)
	said(t, h.Said()) // quiet by default

	h = start(t, map[string]any{"events_on": "follow,gift,sub,first"})
	for _, n := range []string{"ana", "ben", "cy", "dee", "eve"} {
		h.Emit("twitch.channel.follow", map[string]any{"user": n})
	}
	h.Clock.Advance(11 * time.Second)
	said(t, h.Said(), "Welcome to the pack, ana, ben, cy and 2 others!")
	h.Emit("twitch.channel.gift", map[string]any{"user": "fox", "count": 50})
	for i := 0; i < 50; i++ {
		h.Emit("twitch.channel.subscribe", map[string]any{"user": "x", "gift": true})
	}
	said(t, h.Said(), "fox gifted 50 subs! Thank you 🎁")
	said(t, h.Chat("newbie", "hi all"), "Welcome to chat, newbie! 👋")
	said(t, h.Chat("newbie", "hi again"))

	h.Emit("twitch.stream.online", nil)
	said(t, h.Chat("owl", "!so fox", "moderator"), "Welcome to chat, owl! 👋", "Go check out Fox! They were last playing VRChat: https://twitch.tv/fox")
	h.Twitch.Users["wolf"] = &luna.TwitchUser{ID: "id-wolf", Login: "wolf", Name: "Wolf"}
	h.Emit("twitch.channel.raid", map[string]any{"user": "wolf", "viewers": 12})
	said(t, h.Said(), "Go check out Wolf! They were last playing something: https://twitch.tv/wolf")
	if got := h.Twitch.Mods(); !slices.Equal(got, []string{"shoutout id-fox"}) {
		t.Fatalf("native shoutouts wait out Twitch's 2-minute cooldown: %q", got)
	}
	h.Clock.Advance(150 * time.Second) // the queue is checked every 30 s
	if got := h.Twitch.Mods(); !slices.Equal(got, []string{"shoutout id-fox", "shoutout id-wolf"}) {
		t.Fatalf("queued shoutout: %q", got)
	}
}

func TestFold(t *testing.T) {
	for in, want := range map[string]string{"L0ѕT": "lost", "ｆｕｌｌ": "full", "café": "cafe", "z​ero": "zero"} {
		if got := fold(in); got != want {
			t.Errorf("fold(%q) = %q, want %q", in, got, want)
		}
	}
	if got := links("see e.g. this, lol.ok, x.com/y and https://a.b.example.org/z and clips.twitch.tv/x", []string{"clips.twitch.tv"}); !slices.Equal(got, []string{"x.com", "a.b.example.org"}) {
		t.Errorf("links: %q", got)
	}
	if got := joinNames([]string{"a", "b"}); got != "a and b" {
		t.Errorf("joinNames: %q", got)
	}
}
