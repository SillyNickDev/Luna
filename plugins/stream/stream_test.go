package stream

import (
	"slices"
	"strings"
	"testing"
	"time"

	luna "luna/sdk/go"
	"luna/sdk/go/lunatest"
)

func said(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("said %q, want %q", got, want)
	}
}

func TestManifestMatchesCode(t *testing.T) { lunatest.CheckManifest(t, New(), ".") }

func start(t *testing.T, cfg map[string]any) *lunatest.Harness {
	h := lunatest.New(t, New(), ".", cfg)
	h.Twitch.Categories = []luna.Category{{ID: "1", Name: "Elden Ring"}, {ID: "2", Name: "Elden Ring Nightreign"}, {ID: "3", Name: "VRChat"}}
	return h
}

func TestEditTitleAndGame(t *testing.T) {
	h := start(t, nil)
	edit := func(what, value, actor string) string {
		t.Helper()
		out, err := h.Provide("edit", "luna.core", luna.Args{"what": what, "value": value, "actor": actor})
		if err != nil {
			return err.Error()
		}
		return out.(map[string]any)["message"].(string)
	}
	if got := edit("title", "Chill VR night", "mink"); got != "Title set: Chill VR night" {
		t.Fatal(got)
	}
	if got := edit("game", "vrchat", "mink"); got != "Category set: VRChat" {
		t.Fatal(got)
	}
	if got := edit("game", "elden", "mink"); got != "Did you mean Elden Ring? Type !game yes to set it." {
		t.Fatal(got)
	}
	if got := edit("game", "yes", "owl"); got != `No Twitch category matches "yes".` { // only the one who asked can confirm
		t.Fatal(got)
	}
	if got := edit("game", "yes", "mink"); got != "Category set: Elden Ring" {
		t.Fatal(got)
	}
	if got := edit("title", strings.Repeat("x", 141), "mink"); got != "Twitch titles are at most 140 characters." {
		t.Fatal(got)
	}
	want := []string{`set_channel {"game_id":"","title":"Chill VR night"}`, `set_channel {"game_id":"3","title":""}`,
		`set_channel {"game_id":"1","title":""}`}
	if !slices.Equal(h.Twitch.Writes, want) {
		t.Fatalf("writes %q", h.Twitch.Writes)
	}
	if out, err := h.Provide("edit", "luna.core", luna.Args{"what": "title", "value": "x", "actor": "m", "simulated": true}); err != nil ||
		!strings.Contains(out.(map[string]any)["message"].(string), "simulated") || len(h.Twitch.Writes) != 3 {
		t.Fatalf("a simulated edit reached Twitch: %v %v", out, err)
	}
	if _, err := h.CLI("game", luna.Args{"name": "elden"}); err == nil || !strings.Contains(err.Error(), `"Elden Ring", "Elden Ring Nightreign"`) {
		t.Fatalf("cli fuzzy game: %v", err)
	}
}

func TestClipsMarkersAndRecap(t *testing.T) {
	h := start(t, nil)
	said(t, h.Chat("fox", "!clip"), "Clips only work while the stream is live.")
	h.Twitch.Live = luna.Stream{Live: true, Viewers: 42}
	h.Emit("twitch.stream.online", nil)
	h.Twitch.Live.Viewers = 42
	h.Clock.Advance(31 * time.Second)
	said(t, h.Chat("fox", "!clip"), "🎬 https://clips.twitch.tv/clip1")
	said(t, h.Chat("mink", "!marker boss fight", "luna-mod"), "Marker added at 10m0s.")
	h.Emit("twitch.channel.follow", map[string]any{"user": "owl"})
	h.Emit("twitch.channel.gift", map[string]any{"user": "elk", "count": 5})
	h.Emit("twitch.channel.cheer", map[string]any{"user": "fox", "bits": 300})
	h.Chat("fox", "gg")
	h.Twitch.Live.Viewers = 57
	h.Clock.Advance(5 * time.Minute)
	h.Twitch.Live.Viewers = 12
	h.Emit("twitch.stream.offline", nil)

	out, err := h.CLI("recap", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := out.(map[string]any)
	if r["peak_viewers"] != 57.0 || r["gifted_subs"] != 5.0 || r["bits"] != 300.0 || len(r["clips"].([]any)) != 1 ||
		r["markers"].([]any)[0] != "10m0s boss fight" || r["followers"].([]any)[0] != "owl" {
		t.Fatalf("recap %v", r)
	}
	if top := r["top_chatters"].([]any); top[0].(map[string]any)["login"] != "fox" || top[0].(map[string]any)["messages"] != 2.0 {
		t.Fatalf("top chatters %v", top)
	}
}

func TestAdWarningAndThanks(t *testing.T) {
	h := start(t, map[string]any{"ad_warn": true, "ad_thanks": true})
	h.Twitch.Ads = luna.AdSchedule{NextAdAt: h.Clock.Now().Add(10 * time.Minute), DurationS: 90, SnoozeCount: 3}
	h.Twitch.Live = luna.Stream{Live: true}
	h.Emit("twitch.stream.online", nil)
	h.Clock.Advance(8 * time.Minute)
	said(t, h.Said())
	said(t, h.Chat("mink", "!ads snooze", "luna-mod"), "Snoozed: the next ad is in 7m0s. 2 snoozes left.")
	h.Clock.Advance(2 * time.Minute) // the old warning time passes quietly
	said(t, h.Said())
	h.Clock.Advance(4 * time.Minute)
	said(t, h.Said(), "Ads in 1m0s for 1m30s. Grab a drink! 💜")
	h.Emit("twitch.channel.ad_break", map[string]any{"duration_s": 90})
	h.Clock.Advance(90 * time.Second)
	said(t, h.Said(), "Thanks for sitting through the ads! 💜")
}

func TestSchedule(t *testing.T) {
	h := start(t, nil)
	said(t, h.Chat("fox", "!schedule"), "No upcoming streams on the schedule.")
	h.Twitch.Schedule = []luna.Segment{{Start: h.Clock.Now().Add(-time.Hour), Title: "past"},
		{Start: time.Date(2026, 1, 2, 19, 0, 0, 0, time.UTC), Title: "VR night", Category: "VRChat"}}
	h.Clock.Advance(10 * time.Second)
	said(t, h.Chat("fox", "!schedule"), "Next stream: Fri 2 Jan 19:00 UTC, in 30h59m50s: VR night (VRChat). !schedule tz <zone> to see it in your time.")
	h.Clock.Advance(10 * time.Second)
	said(t, h.Chat("fox", "!schedule tz Mars/Olympus"), `"Mars/Olympus" isn't a time zone I know; try one like Europe/London.`)
	h.Clock.Advance(10 * time.Second)
	said(t, h.Chat("fox", "!schedule tz America/New_York"), "Got it: I'll show the schedule in America/New_York time.")
	h.Clock.Advance(10 * time.Second)
	said(t, h.Chat("fox", "!schedule"), "Next stream: Fri 2 Jan 14:00 EST, in 30h59m20s: VR night (VRChat)")
}

func TestRaid(t *testing.T) {
	h := start(t, map[string]any{"raid_friends": "wolf, Owl,elk"})
	h.Twitch.Streams = map[string]luna.LiveStream{"wolf": {Login: "wolf", Game: "VRChat", Viewers: 80},
		"owl": {Login: "owl", Game: "Art", Viewers: 12}}
	said(t, h.Chat("mink", "!raid", "luna-mod"), "Live friends: owl (Art, 12) · wolf (VRChat, 80). !raid <name> to go.")
	h.Twitch.Users["owl"] = &luna.TwitchUser{ID: "123", Login: "owl", Name: "Owl"}
	said(t, h.Chat("mink", "!raid @owl", "luna-mod"), "Raiding Owl in 90 seconds! (!raid cancel to stop)")
	said(t, h.Chat("mink", "!raid cancel", "luna-mod"), "Raid cancelled.")
	said(t, h.Chat("fox", "!raid owl")) // viewers lack stream.edit
	if !slices.Equal(h.Twitch.Writes, []string{`raid {"to_id":"123"}`, "unraid"}) {
		t.Fatalf("writes %q", h.Twitch.Writes)
	}
}
