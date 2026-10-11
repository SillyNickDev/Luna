package queue

import (
	"slices"
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

func TestQueue(t *testing.T) {
	h := lunatest.New(t, New(), ".", map[string]any{"sub_priority": true, "group_size": 2,
		"invite_message": "You're up, {name}! Join Winterpaw in VRChat."})
	said(t, h.Chat("fox", "!join"), "The queue is closed.")
	said(t, h.Chat("mink", "!queue open", "moderator"), "The queue is open: !join to play!")
	said(t, h.Chat("fox", "!queue close")) // viewers can't
	h.Chat("fox", "!join FoxVRC")
	h.Clock.Advance(time.Second)
	h.Chat("owl", "!join")
	h.Clock.Advance(time.Second)
	h.Chat("elk", "!join ElkVR", "sub") // subs go first
	h.Chat("fox", "!join FoxNew")       // already in: keeps the place, new name
	h.Clock.Advance(5 * time.Second)
	said(t, h.Chat("owl", "!queue"), "owl, you're #3. 3 waiting: elk (ElkVR), fox (FoxNew), owl")
	h.Chat("owl", "!leave")

	said(t, h.Chat("mink", "!next", "moderator"), "You're up, elk! Join Winterpaw in VRChat.", // whispered, checked below
		"You're up, fox! Join Winterpaw in VRChat.", "🎮 Up next: @elk (ElkVR), @fox (FoxNew)")
	var whispers []string
	for _, m := range h.Messages() {
		if m.Mode == "whisper" {
			whispers = append(whispers, m.To+": "+m.Text)
		}
	}
	if !slices.Equal(whispers, []string{"id-elk: You're up, elk! Join Winterpaw in VRChat.", "id-fox: You're up, fox! Join Winterpaw in VRChat."}) {
		t.Fatalf("whispers %q", whispers)
	}
	said(t, h.Chat("fox", "!join"), "fox, you just played: you can rejoin in 30m0s.")
	h.Clock.Advance(31 * time.Minute)
	said(t, h.Chat("fox", "!join"))
	said(t, h.Chat("mink", "!next 5", "moderator"), "You're up, fox! Join Winterpaw in VRChat.", "🎮 Up next: @fox")
	said(t, h.Chat("mink", "!next", "moderator"), "Nobody is in the queue.")
	said(t, h.Chat("fox", "!next")) // viewers lack queue.run
}

func TestMaxSizeAndClear(t *testing.T) {
	h := lunatest.New(t, New(), ".", map[string]any{"max_size": 1})
	h.Chat("mink", "!queue open", "moderator")
	h.Chat("fox", "!join")
	said(t, h.Chat("owl", "!join"), "The queue is full.")
	h.Clock.Advance(5 * time.Second)
	said(t, h.Chat("mink", "!queue clear", "moderator"), "The queue is empty.")
	if out, _ := h.CLI("list", nil); out != nil && len(out.([]any)) != 0 {
		t.Fatalf("list after clear: %v", out)
	}
}
