package points

import (
	"errors"
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

func TestWatchTimeOnlyWhileLiveAndChatting(t *testing.T) {
	h := lunatest.New(t, New(), ".", map[string]any{"name": "bones", "emoji": "🦴"})
	h.Chat("fox", "hi before the stream")
	h.Clock.Advance(5 * time.Minute)
	said(t, h.Chat("fox", "!points"), "fox, you have 0 bones 🦴.")

	h.Emit("twitch.stream.online", nil)
	h.Chat("fox", "hi")
	h.Chat("owl", "hello", "sub")
	h.Clock.Advance(10 * time.Minute) // two payouts: fox chatted inside the window for both
	h.Clock.Advance(10 * time.Minute) // one more at 15 min, then fox has gone quiet
	said(t, h.Chat("fox", "!points"), "fox, you have 20 bones 🦴.")
	said(t, h.Chat("owl", "!points", "sub"), "owl, you have 30 bones 🦴.")

	h.Emit("twitch.stream.offline", nil)
	h.Clock.Advance(time.Hour)
	said(t, h.Chat("fox", "!points fox"), "fox has 20 bones 🦴.")
}

func TestBonuses(t *testing.T) {
	h := lunatest.New(t, New(), ".", nil)
	h.Emit("twitch.channel.follow", map[string]any{"user": "fox"})
	h.Emit("twitch.channel.cheer", map[string]any{"user": "fox", "bits": 250})
	h.Emit("twitch.channel.cheer", map[string]any{"user": "fox", "bits": 1000, "anonymous": true})
	h.Emit("twitch.channel.subscribe", map[string]any{"user": "owl", "gift": true}) // the gifter is paid instead
	h.Emit("twitch.channel.gift", map[string]any{"user": "elk", "count": 3})
	said(t, h.Chat("fox", "!points"), "fox, you have 75 points.")
	said(t, h.Chat("owl", "!points"), "owl, you have 0 points.")
	said(t, h.Chat("elk", "!points"), "elk, you have 300 points.")
}

func TestGiveAdjustUndo(t *testing.T) {
	h := lunatest.New(t, New(), ".", nil)
	h.Emit("twitch.channel.follow", map[string]any{"user": "fox"})
	h.Chat("owl", "hi") // owl gets a viewer profile, so !give can find them
	said(t, h.Chat("fox", "!give owl 20"), "fox gave 20 points to owl.")
	h.Clock.Advance(5 * time.Second) // !give's per-user cooldown
	said(t, h.Chat("fox", "!give owl 500"), "not enough points: fox has 30 points")
	h.Clock.Advance(5 * time.Second)
	said(t, h.Chat("fox", "!give owl -5"), "Usage: !give <user> <amount>")
	said(t, h.Chat("fox", "!points"), "fox, you have 30 points.")

	said(t, h.Chat("fox", "!points add fox 1000")) // viewers can't
	said(t, h.Chat("mink", "!points add owl 100", "luna-mod"), "owl now has 120 points.")
	h.Clock.Advance(5 * time.Second)
	said(t, h.Chat("mink", "!points remove owl 500", "luna-mod"), "not enough points: owl has 120 points")

	led, err := h.CLI("ledger", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rows := led.([]any); len(rows) != 2 || rows[0].(map[string]any)["actor"] != "mink" {
		t.Fatalf("ledger %v", led)
	}
	h.Clock.Advance(5 * time.Second)
	said(t, h.Chat("mink", "!points undo 1", "luna-mod"), "Undid #1 (give by fox).")
	h.Clock.Advance(5 * time.Second)
	said(t, h.Chat("mink", "!points undo 1", "luna-mod"), "#1 is already undone")
	h.Clock.Advance(5 * time.Second)
	said(t, h.Chat("fox", "!points"), "fox, you have 50 points.")
	said(t, h.Chat("owl", "!points"), "owl, you have 100 points.")

	top, _ := h.CLI("top", nil)
	if rows := top.([]any); len(rows) != 2 || rows[0].(map[string]any)["login"] != "owl" {
		t.Fatalf("top %v", top)
	}
	said(t, h.Chat("fox", "!top"), "Top points: 1. owl 100 · 2. fox 50")
}

func TestLinks(t *testing.T) {
	h := lunatest.New(t, New(), ".", nil)
	h.Emit("twitch.channel.follow", map[string]any{"user": "fox"})
	out, err := h.Provide("spend", "luna.giveaways", luna.Args{"user_id": "id-fox", "amount": 30, "reason": "tickets"})
	if err != nil || out.(map[string]any)["balance"] != 20.0 {
		t.Fatalf("spend: %v %v", out, err)
	}
	if _, err := h.Provide("spend", "luna.giveaways", luna.Args{"user_id": "id-fox", "amount": 21}); !errors.Is(err, errNotEnough) {
		t.Fatalf("overspend: %v", err)
	}
	if _, err := h.Provide("add", "luna.giveaways", luna.Args{"user_id": "id-fox", "amount": -5}); err == nil {
		t.Fatal("negative add accepted")
	}
	out, _ = h.Provide("add", "luna.giveaways", luna.Args{"user_id": "id-fox", "amount": 5, "reason": "refund"})
	if out.(map[string]any)["balance"] != 25.0 {
		t.Fatalf("add: %v", out)
	}
	led, _ := h.CLI("ledger", nil)
	if e := led.([]any)[1].(map[string]any); e["actor"] != "luna.giveaways" || e["reason"] != "tickets" {
		t.Fatalf("spend not in the ledger as the caller: %v", e)
	}
}

func TestStreakShout(t *testing.T) {
	h := lunatest.New(t, New(), ".", map[string]any{"streak_shout": true})
	for i := range 5 {
		h.Emit("twitch.stream.online", nil)
		got := h.Chat("fox", "hi")
		h.Chat("fox", "again") // once per stream
		h.Emit("twitch.stream.offline", nil)
		if i < 4 {
			said(t, got)
		} else {
			said(t, got, "fox has been here 5 streams in a row! 🔥")
		}
	}
}
