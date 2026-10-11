package polls

import (
	"slices"
	"strings"
	"testing"
	"time"

	"luna/sdk/go/lunatest"
)

func said(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("said %q, want %q", got, want)
	}
}

func TestManifestMatchesCode(t *testing.T) { lunatest.CheckManifest(t, New(), ".") }

func TestNativePoll(t *testing.T) {
	h := lunatest.New(t, New(), ".", nil)
	said(t, h.Chat("fox", "!poll Pizza? | yes | no")) // viewers lack polls.run
	said(t, h.Chat("mink", "!poll Pizza or tacos? | Pizza | Tacos duration=1m", "moderator"))
	if w := h.Twitch.Writes; len(w) != 1 || !strings.Contains(w[0], `"duration_s":60`) {
		t.Fatalf("writes %q", w)
	}
	said(t, h.Chat("mink", "!poll Again? | a | b", "moderator"), "A poll is already running: !poll end first.")
	h.Twitch.Polls["poll1"].Choices[0].Votes, h.Twitch.Polls["poll1"].Choices[1].Votes = 3, 1
	said(t, h.Chat("mink", "!poll end", "moderator"), "📊 Pizza or tacos?: Pizza 3 (75%) · Tacos 1 (25%). Winner: Pizza")
}

func TestChatPollFallback(t *testing.T) {
	h := lunatest.New(t, New(), ".", nil)
	h.Twitch.NoPolls = true
	said(t, h.Chat("mink", "!poll Snack? | Chips | Grapes | Toast duration=30s", "moderator"),
		"📊 Snack? 1) Chips 2) Grapes 3) Toast. Vote with !vote <number> for 30s. (Twitch polls aren't available: twitch: 403 The broadcaster must be a partner or affiliate)")
	h.Chat("fox", "!vote 1")
	h.Chat("owl", "!vote 2")
	h.Chat("fox", "!vote 2") // changed their mind
	h.Chat("elk", "!vote 9") // no such option
	h.Clock.Advance(30 * time.Second)
	said(t, h.Said(), "📊 Snack?: Chips 0 (0%) · Grapes 2 (100%) · Toast 0 (0%). Winner: Grapes")
	var updates int
	for _, e := range h.Events {
		if e.Type == "plugin.luna.polls.update" {
			updates++
		}
	}
	if updates != 5 { // start, three counted votes, end
		t.Fatalf("%d overlay updates", updates)
	}
	said(t, h.Chat("mink", "!poll end", "moderator"), "No poll is running.")
}

func TestPollValidation(t *testing.T) {
	h := lunatest.New(t, New(), ".", nil)
	said(t, h.Chat("mink", "!poll Lonely? | yes", "moderator"), "give a question and at least two options, split by |")
	said(t, h.Chat("mink", "!poll Q | a | b duration=5s", "moderator"), "A poll runs 15 seconds to 30 minutes.")
	said(t, h.Chat("mink", "!poll Q | a | b | c | d | e | f", "moderator"), "A poll has 2 to 5 options.")
	said(t, h.Chat("mink", "!poll Q | "+strings.Repeat("x", 26)+" | b", "moderator"),
		`"`+strings.Repeat("x", 26)+`" is over Twitch's 25 characters for an option.`)
}

func TestPrediction(t *testing.T) {
	h := lunatest.New(t, New(), ".", nil)
	said(t, h.Chat("mink", "!predict win 1", "moderator"), "No prediction is running.")
	said(t, h.Chat("mink", "!predict Will I die to the boss? | Yes | No", "moderator"))
	said(t, h.Chat("mink", "!predict lock", "moderator"))
	said(t, h.Chat("mink", "!predict win 3", "moderator"), "Usage: !predict win <1-2>")
	said(t, h.Chat("mink", "!predict win 1", "moderator"))
	if w := h.Twitch.Writes; len(w) != 3 || !strings.Contains(w[2], `"status":"RESOLVED"`) || !strings.Contains(w[2], `"winner":"o1"`) {
		t.Fatalf("writes %q", w)
	}
	h.Twitch.NoPolls = true
	said(t, h.Chat("mink", "!predict Q | a | b", "moderator"),
		"Twitch predictions aren't available: twitch: 403 The broadcaster must be a partner or affiliate")
}
