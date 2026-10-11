package hrc

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

func hr(h *lunatest.Harness, bpm int) { h.Emit("hr.update", map[string]any{"bpm": bpm}) }

func TestHRCommandAndHiding(t *testing.T) {
	h := lunatest.New(t, New(), ".", map[string]any{"name": "Winterpaw"})
	said(t, h.Chat("fox", "!hr"), "No heart rate reading right now.")
	hr(h, 92)
	h.Clock.Advance(11 * time.Second)
	said(t, h.Chat("fox", "!heartrate"), "Winterpaw's heart rate: 92 bpm 💓")
	if got := h.OSC.TakeSent(); !slices.Equal(got, []string{"/avatar/parameters/HeartRate 92"}) {
		t.Fatalf("osc %q", got)
	}
	said(t, h.Chat("mink", "!hr off", "moderator")) // only the broadcaster
	h.Clock.Advance(11 * time.Second)
	said(t, h.Chat("me", "!hr off", "broadcaster"), "Heart rate hidden. !hr on to show it again.")
	hr(h, 95)
	h.Clock.Advance(11 * time.Second)
	said(t, h.Chat("fox", "!hr"))
	if got := h.OSC.TakeSent(); len(got) != 0 {
		t.Fatalf("hidden but sent %q", got)
	}
	h.Clock.Advance(11 * time.Second)
	said(t, h.Chat("me", "!hr on", "broadcaster"), "Heart rate showing again.")
	h.Clock.Advance(70 * time.Second)
	said(t, h.Chat("fox", "!hr"), "No heart rate reading right now.") // stale
}

func TestAlertsOnCrossing(t *testing.T) {
	h := lunatest.New(t, New(), ".", map[string]any{"alert": true, "alert_bpm": 140, "alert_cooldown_s": 60})
	for _, b := range []int{120, 141, 150, 139, 145} {
		hr(h, b)
	}
	said(t, h.Said(), "The streamer's heart rate just hit 141 bpm 💓") // 145 recrossed inside the cooldown
	h.Clock.Advance(time.Minute)
	hr(h, 130)
	hr(h, 160)
	said(t, h.Said(), "The streamer's heart rate just hit 160 bpm 💓")
}

func TestSummary(t *testing.T) {
	h := lunatest.New(t, New(), ".", map[string]any{"summary_chat": true, "osc_param": ""})
	hr(h, 200) // before the stream: not counted
	h.Emit("twitch.stream.online", nil)
	for i, b := range []int{70, 72, 75, 80, 90, 120, 155, 100, 85, 80} {
		h.Clock.Advance(time.Duration(i) * time.Minute)
		hr(h, b)
	}
	h.Emit("twitch.stream.offline", nil)
	said(t, h.Said(), "Heart rate this stream: resting 72, average 92, peak 155 bpm 21m0s in.")
	out, err := h.CLI("summary", nil)
	if err != nil || out.(map[string]any)["peak_bpm"] != 155.0 || out.(map[string]any)["readings"] != 10.0 {
		t.Fatalf("%v %v", out, err)
	}
	if len(h.OSC.Sent) != 0 {
		t.Fatalf("osc_param is off but sent %q", h.OSC.Sent)
	}
}
