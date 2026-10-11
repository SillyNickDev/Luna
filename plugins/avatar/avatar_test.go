package avatar

import (
	"slices"
	"strings"
	"testing"
	"time"

	"luna/internal/osc"
	luna "luna/sdk/go"
	"luna/sdk/go/lunatest"
)

func TestManifestMatchesCode(t *testing.T) { lunatest.CheckManifest(t, New(), ".") }

var winterpaw = osc.Avatar{ID: "avtr_1", Name: "Winterpaw", Params: []osc.Param{
	{Name: "Ears", Type: "Bool", Writable: true}, {Name: "Sparkles", Type: "Bool", Writable: true},
	{Name: "Outfit", Type: "Int", Writable: true}, {Name: "Glow", Type: "Float", Writable: true}, {Name: "VelocityX", Type: "Float"}}}

func start(t *testing.T) *lunatest.Harness {
	h := lunatest.New(t, New(), ".", map[string]any{
		"redeem": []map[string]any{
			{"reward": "Toggle ears", "param": "Ears", "action": "toggle"},
			{"reward": "Sparkle burst", "param": "Sparkles", "action": "pulse", "duration": "5s"},
			{"reward": "Glow up", "param": "Glow", "action": "set", "value": 1},
			{"reward": "Fox hat", "param": "Hat", "action": "toggle", "avatar": "Fox"},
		},
		"bits": []map[string]any{
			{"min": 100, "param": "Sparkles", "action": "pulse", "duration": "2s"},
			{"min": 500, "param": "Outfit", "action": "set", "value": 3},
		},
		"chatbox": "say", "chatbox_reward": "Say it",
	})
	h.OSC.Av = winterpaw
	return h
}

func redeem(h *lunatest.Harness, reward string) {
	h.Emit("twitch.channel.redeem", map[string]any{"user": "fox", "reward": reward, "input": "hello VR"})
}

func want(t *testing.T, h *lunatest.Harness, sent ...string) {
	t.Helper()
	if got := h.OSC.TakeSent(); !slices.Equal(got, sent) {
		t.Fatalf("sent %q, want %q", got, sent)
	}
}

func TestRedeems(t *testing.T) {
	h := start(t)
	redeem(h, "Toggle ears")
	redeem(h, "toggle EARS")
	want(t, h, "/avatar/parameters/Ears true", "/avatar/parameters/Ears false")
	h.OSC.Params["Ears"] = true // VRChat's value wins over Luna's memory
	redeem(h, "Toggle ears")
	want(t, h, "/avatar/parameters/Ears false")

	redeem(h, "Sparkle burst")
	want(t, h, "/avatar/parameters/Sparkles true")
	h.Clock.Advance(5 * time.Second)
	want(t, h, "/avatar/parameters/Sparkles false")

	redeem(h, "Glow up")
	want(t, h, "/avatar/parameters/Glow 1") // a Float, since the avatar says so
	redeem(h, "Fox hat")                    // for another avatar
	redeem(h, "Unmapped")
	want(t, h)
}

func TestBits(t *testing.T) {
	h := start(t)
	h.Emit("twitch.channel.cheer", map[string]any{"user": "fox", "bits": 50})
	want(t, h)
	h.Emit("twitch.channel.cheer", map[string]any{"user": "fox", "bits": 100})
	want(t, h, "/avatar/parameters/Sparkles true")
	h.Clock.Advance(2 * time.Second)
	h.OSC.TakeSent()
	h.Emit("twitch.channel.cheer", map[string]any{"user": "fox", "bits": 600}) // only the biggest threshold reached
	want(t, h, "/avatar/parameters/Outfit 3")
}

// After switching to an avatar without the parameter, nothing is sent.
func TestAvatarChange(t *testing.T) {
	h := start(t)
	h.OSC.Av = osc.Avatar{ID: "avtr_2", Name: "Fox", Params: []osc.Param{{Name: "Hat", Type: "Bool", Writable: true}}}
	h.Emit("osc.avatar", map[string]any{"id": "avtr_2", "name": "Fox"})
	redeem(h, "Toggle ears")
	want(t, h)
	redeem(h, "Fox hat")
	want(t, h, "/avatar/parameters/Hat true")

	out, err := h.CLI("params", nil)
	if err != nil {
		t.Fatal(err)
	}
	probs := out.(map[string]any)["problems"].([]any)
	if len(probs) != 5 || !strings.Contains(probs[0].(string), `no settable parameter "Ears"`) {
		t.Fatalf("problems %q", probs)
	}
}

func TestChatbox(t *testing.T) {
	h := start(t)
	redeem(h, "Say it")
	h.Chat("owl", "hi there", "sub") // mode is "say": subs aren't mirrored
	if !slices.Equal(h.OSC.Chatboxes, []string{"fox: hello VR"}) {
		t.Fatalf("chatbox %q", h.OSC.Chatboxes)
	}

	h = lunatest.New(t, New(), ".", map[string]any{"chatbox": "subs"})
	h.Chat("owl", "hi there", "sub")
	h.Chat("owl", "!points", "sub") // commands stay out of the chatbox
	h.Chat("fox", "not a sub")
	if !slices.Equal(h.OSC.Chatboxes, []string{"owl: hi there"}) {
		t.Fatalf("chatbox %q", h.OSC.Chatboxes)
	}
}

func TestProblems(t *testing.T) {
	h := lunatest.New(t, New(), ".", map[string]any{"redeem": []map[string]any{
		{"reward": "A", "param": "Ears", "action": "flip"},
		{"reward": "B", "action": "toggle"},
		{"reward": "C", "param": "Outfit", "action": "set"},
		{"reward": "D", "param": "Ears", "action": "pulse", "duration": "soon"},
	}})
	out, _ := h.CLI("params", nil)
	got := out.(map[string]any)["problems"].([]any)
	for i, want := range []string{`action "flip" isn't toggle, pulse or set`, "no param", "set needs a value", `duration "soon"`} {
		if !strings.Contains(got[i].(string), want) {
			t.Errorf("problem %d: %q, want %q", i, got[i], want)
		}
	}
	if _, err := h.CLI("test", luna.Args{"reward": "nope"}); err == nil {
		t.Error("testing an unmapped reward")
	}
}
