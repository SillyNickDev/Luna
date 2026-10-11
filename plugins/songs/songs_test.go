package songs

import (
	"errors"
	"fmt"
	"slices"
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
	h.Emit("twitch.stream.online", nil)
	for _, tr := range []*luna.Track{
		{ID: "t1", URI: "spotify:track:t1", Name: "Never Gonna Give You Up", Artists: "Rick Astley", DurationMS: 213000},
		{ID: "t2", URI: "spotify:track:t2", Name: "Long Mix", Artists: "DJ", DurationMS: 900000},
		{ID: "t3", URI: "spotify:track:t3", Name: "Rude Song", Artists: "Band", DurationMS: 180000, Explicit: true},
		{ID: "t4", URI: "spotify:track:t4", Name: "Darude Sandstorm", Artists: "Darude", DurationMS: 220000},
	} {
		h.Spotify.Tracks[tr.ID] = tr
	}
	return h
}

func TestRequests(t *testing.T) {
	h := start(t, map[string]any{"no_explicit": true, "blocklist": "sandstorm", "max_per_user": 1})
	said(t, h.Chat("fox", "!sr never gonna"), "🎵 Added Never Gonna Give You Up by Rick Astley (#1 in the queue).")
	said(t, h.Chat("owl", "!sr https://open.spotify.com/track/t2"), "Can't request Long Mix by DJ: it's longer than 10m0s.")
	h.Clock.Advance(10 * time.Second)
	said(t, h.Chat("owl", "!sr rude song"), "Can't request Rude Song by Band: it's explicit.")
	h.Clock.Advance(10 * time.Second)
	said(t, h.Chat("owl", "!sr darude"), "Can't request Darude Sandstorm by Darude: it's on the blocklist.")
	h.Clock.Advance(10 * time.Second)
	said(t, h.Chat("fox", "!sr rick"), "fox, you already have 1 songs waiting.")
	h.Clock.Advance(10 * time.Second)
	said(t, h.Chat("owl", "!sr nothing like this"), `No song found for "nothing like this".`)
	if !slices.Equal(h.Spotify.Queued, []string{"spotify:track:t1"}) {
		t.Fatalf("queued %v", h.Spotify.Queued)
	}

	h.Spotify.Now = h.Spotify.Tracks["t1"]
	h.Clock.Advance(15 * time.Second) // the watcher sees it start
	said(t, h.Chat("elk", "!song"), "🎵 Now playing: Never Gonna Give You Up by Rick Astley (requested by fox)")
	if out, _ := h.CLI("list", nil); len(out.([]any)) != 0 {
		t.Fatalf("still waiting: %v", out)
	}
	said(t, h.Chat("mink", "!skip", "moderator"))
	said(t, h.Chat("elk", "!skip")) // viewers lack songs.skip
	if h.Spotify.Skips != 1 {
		t.Fatalf("skips %d", h.Spotify.Skips)
	}
}

func TestWrongSongAndCost(t *testing.T) {
	h := start(t, map[string]any{"cost": 50})
	bal := map[string]int64{"id-fox": 60}
	h.Link(spend, func(a luna.Args) (any, error) {
		if bal[a.Str("user_id")] < a.Int("amount") {
			return nil, errors.New("not enough points: fox has " + fmt.Sprint(bal[a.Str("user_id")]))
		}
		bal[a.Str("user_id")] -= a.Int("amount")
		return nil, nil
	})
	h.Link(add, func(a luna.Args) (any, error) { bal[a.Str("user_id")] += a.Int("amount"); return nil, nil })
	said(t, h.Chat("fox", "!sr never gonna"), "🎵 Added Never Gonna Give You Up by Rick Astley (#1 in the queue).")
	h.Clock.Advance(10 * time.Second)
	said(t, h.Chat("fox", "!sr rude"), "not enough points: fox has 10")
	said(t, h.Chat("fox", "!wrongsong"), "Got it: Never Gonna Give You Up by Rick Astley will be skipped when it comes up.")
	if bal["id-fox"] != 60 {
		t.Fatalf("not refunded: %v", bal)
	}
	h.Spotify.Now = h.Spotify.Tracks["t1"]
	h.Clock.Advance(15 * time.Second)
	said(t, h.Said(), "Skipped Never Gonna Give You Up by Rick Astley (wrong song).")
	if h.Spotify.Skips != 1 {
		t.Fatalf("skips %d", h.Spotify.Skips)
	}
}

func TestPlaylistAndRedeem(t *testing.T) {
	h := start(t, map[string]any{"playlist": "https://open.spotify.com/playlist/p1", "chat_requests": false, "reward": "Song request"})
	h.Spotify.Playlists["https://open.spotify.com/playlist/p1"] = []string{"t4"}
	said(t, h.Chat("fox", "!sr rick"), `Request songs with the "Song request" channel point reward.`)
	h.Emit("twitch.channel.redeem", map[string]any{"user": "fox", "reward": "Song request", "input": "never gonna"})
	said(t, h.Said(), "Can't request Never Gonna Give You Up by Rick Astley: only songs from the request playlist are allowed.")
	h.Emit("twitch.channel.redeem", map[string]any{"user": "fox", "reward": "song REQUEST", "input": "darude"})
	said(t, h.Said(), "🎵 Added Darude Sandstorm by Darude (#1 in the queue).")
}

// Requests that never play stop counting: at the end of the stream, or after 3 hours.
func TestStaleRequests(t *testing.T) {
	h := start(t, map[string]any{"max_per_user": 1})
	said(t, h.Chat("fox", "!sr never gonna"), "🎵 Added Never Gonna Give You Up by Rick Astley (#1 in the queue).")
	h.Clock.Advance(3*time.Hour + time.Minute)
	said(t, h.Chat("fox", "!sr darude"), "🎵 Added Darude Sandstorm by Darude (#1 in the queue).")
	h.Emit("twitch.stream.offline", nil)
	if out, _ := h.CLI("list", nil); len(out.([]any)) != 0 {
		t.Fatalf("still waiting after the stream: %v", out)
	}
}
