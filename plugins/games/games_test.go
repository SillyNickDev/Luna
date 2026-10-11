package games

import (
	"context"
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

// points fakes the Points plugin's spend and add links over a balance map.
func points(h *lunatest.Harness, bal map[string]int64) {
	h.Link(spend, func(a luna.Args) (any, error) {
		if bal[a.Str("user_id")] < a.Int("amount") {
			return nil, fmt.Errorf("not enough points: %s has %d", a.Str("login"), bal[a.Str("user_id")])
		}
		bal[a.Str("user_id")] -= a.Int("amount")
		return map[string]any{"balance": bal[a.Str("user_id")]}, nil
	})
	h.Link(add, func(a luna.Args) (any, error) {
		bal[a.Str("user_id")] += a.Int("amount")
		return map[string]any{"balance": bal[a.Str("user_id")]}, nil
	})
}

func start(t *testing.T, cfg map[string]any) (*lunatest.Harness, map[string]int64) {
	h := lunatest.New(t, New(), ".", cfg)
	h.Emit("twitch.stream.online", nil)
	bal := map[string]int64{"id-fox": 500, "id-owl": 500, "id-elk": 500}
	points(h, bal)
	t.Cleanup(func() { roll = nil })
	return h, bal
}

func fixed(n int) { roll = func(int) int { return n } }

func TestDuel(t *testing.T) {
	h, bal := start(t, nil)
	said(t, h.Chat("fox", "!duel fox 10"), "You can't duel yourself.")
	h.Clock.Advance(10 * time.Second)
	said(t, h.Chat("fox", "!duel @owl 5000"), "the most you can bet is 1000")
	h.Clock.Advance(10 * time.Second)
	said(t, h.Chat("fox", "!duel owl 100"), "fox challenges owl to a duel for 100 points! owl, type !accept within 30s.")
	said(t, h.Chat("elk", "!accept")) // not challenged
	fixed(1)                          // the accepter wins
	said(t, h.Chat("owl", "!accept"), "⚔️ owl beats fox and wins 200 points!")
	if bal["id-fox"] != 400 || bal["id-owl"] != 600 {
		t.Fatalf("balances %v", bal)
	}

	h.Clock.Advance(10 * time.Second)
	h.Chat("fox", "!duel elk 50")
	h.Clock.Advance(31 * time.Second)
	said(t, h.Said(), "elk didn't accept the duel; fox gets their 50 points back.")
	if bal["id-fox"] != 400 {
		t.Fatalf("not refunded: %v", bal)
	}
	if keys, _ := h.Store.PluginKeys("luna.games", "stake:"); len(keys) != 0 {
		t.Fatalf("stakes left behind: %v", keys)
	}
}

func TestDuelAccepterCantPay(t *testing.T) {
	h, bal := start(t, nil)
	bal["id-owl"] = 10
	h.Chat("fox", "!duel owl 100")
	said(t, h.Chat("owl", "!accept"), "not enough points: owl has 10 The duel is off.")
	if bal["id-fox"] != 500 || bal["id-owl"] != 10 {
		t.Fatalf("balances %v", bal)
	}
}

func TestHeist(t *testing.T) {
	h, bal := start(t, nil)
	said(t, h.Chat("fox", "!heist 100"), "💰 fox is planning a heist! Join with !heist <points> in the next 1m0s.")
	said(t, h.Chat("owl", "!heist 200"))
	said(t, h.Chat("owl", "!heist 200")) // already in
	fixed(34)                            // 30% + 5% for the second member: 34 < 35 wins
	h.Clock.Advance(time.Minute)
	said(t, h.Said(), "🎉 The heist worked (35% chance)! fox, owl each get 200% of their bet back.")
	if bal["id-fox"] != 600 || bal["id-owl"] != 700 {
		t.Fatalf("balances %v", bal)
	}

	h.Clock.Advance(10 * time.Second)
	h.Chat("elk", "!heist 100")
	fixed(30) // alone: 30% chance, 30 is a miss
	h.Clock.Advance(time.Minute)
	said(t, h.Said(), "🚨 The heist failed (30% chance). elk lost their bets.")
	if bal["id-elk"] != 400 {
		t.Fatalf("balances %v", bal)
	}
}

func TestTrivia(t *testing.T) {
	h, bal := start(t, map[string]any{"trivia": []map[string]any{
		{"question": "Capital of Australia?", "answer": "Canberra|canberra city", "reward": 50},
	}})
	fixed(0)
	said(t, h.Chat("fox", "!trivia"), "❓ Capital of Australia? (first right answer wins 50 points)")
	said(t, h.Chat("owl", "sydney"))
	said(t, h.Chat("elk", " CANBERRA "), "✅ elk got it: Canberra! +50 points")
	said(t, h.Chat("owl", "canberra")) // already answered
	if bal["id-elk"] != 550 {
		t.Fatalf("balances %v", bal)
	}
	h.Clock.Advance(time.Minute)
	h.Said()
	h.Chat("fox", "!trivia")
	h.Clock.Advance(31 * time.Second)
	said(t, h.Said(), "⏰ Time's up! The answer was: Canberra")
}

func TestClosed(t *testing.T) {
	h := lunatest.New(t, New(), ".", map[string]any{"disabled_categories": "Phasmophobia, Elden Ring"})
	said(t, h.Chat("fox", "!heist 10"), "Games only run while the stream is live.")
	h.Emit("twitch.stream.online", nil)
	h.Emit("twitch.channel.update", map[string]any{"game": "elden ring"})
	h.Clock.Advance(10 * time.Second)
	said(t, h.Chat("fox", "!heist 10"), "Games are off during elden ring.")
	h.Emit("twitch.channel.update", map[string]any{"game": "VRChat"})
	h.Clock.Advance(10 * time.Second)
	said(t, h.Chat("fox", "!heist 10"), "Games need the Points plugin running.")
}

// A restart refunds the stakes of games it interrupted.
func TestRefundOnStart(t *testing.T) {
	b, g := build()
	h := lunatest.New(t, b, ".", nil)
	bal := map[string]int64{"id-fox": 0}
	points(h, bal)
	h.Store.PluginSet("luna.games", "stake:7", []byte(`{"user_id":"id-fox","login":"fox","amount":120,"game":"heist"}`), 0)
	if err := g.refundInterrupted(&luna.Ctx{Context: context.Background()}); err != nil {
		t.Fatal(err)
	}
	if keys, _ := h.Store.PluginKeys("luna.games", "stake:"); bal["id-fox"] != 120 || len(keys) != 0 {
		t.Fatalf("balance %v, stakes left %v", bal, keys)
	}
}
