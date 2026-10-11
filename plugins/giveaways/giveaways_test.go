package giveaways

import (
	"encoding/json"
	"errors"
	"fmt"
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

func saidPrefix(t *testing.T, got []string, prefix string) string {
	t.Helper()
	if len(got) != 1 || !strings.HasPrefix(got[0], prefix) {
		t.Fatalf("said %q, want one message starting %q", got, prefix)
	}
	return got[0]
}

func TestManifestMatchesCode(t *testing.T) { lunatest.CheckManifest(t, New(), ".") }

func live(t *testing.T, cfg map[string]any) *lunatest.Harness {
	h := lunatest.New(t, New(), ".", cfg)
	h.Emit("twitch.stream.online", nil)
	return h
}

func export(t *testing.T, h *lunatest.Harness) *Export {
	t.Helper()
	out, err := h.CLI("export", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	var e Export
	json.Unmarshal(b, &e)
	return &e
}

func TestKeywordGiveaway(t *testing.T) {
	h := lunatest.New(t, New(), ".", nil)
	said(t, h.Chat("mink", "!giveaway start Keychain", "luna-mod"),
		"Giveaways only run while you're live (set plugins.giveaways.live_only = false to change that).")
	said(t, h.Chat("fox", "!giveaway start Keychain")) // viewers lack giveaway.run
	h.Emit("twitch.stream.online", nil)

	msg := saidPrefix(t, h.Chat("mink", `!giveaway start "Winterpaw keychain" duration=5m`, "luna-mod"),
		"🎁 Giveaway: Winterpaw keychain! type !enter to enter, for 5m0s. Fair-draw commitment: ")
	commit := strings.TrimSuffix(msg[strings.LastIndex(msg, " ")+1:], "…")
	if e := export(t, h); e.Giveaway.Seed != "" || !strings.HasPrefix(e.Giveaway.Commit, commit) {
		t.Fatalf("seed leaked before the draw, or commit mismatch: %+v", e.Giveaway)
	}

	said(t, h.Chat("fox", "!enter"))
	said(t, h.Chat("fox", "!enter")) // entering twice changes nothing
	said(t, h.Chat("owl", "!ENTER please", "sub"))
	said(t, h.Chat("elk", "!enter", "vip"))
	said(t, h.Chat("doberman", "!enter", "broadcaster"), "You can't enter your own giveaway.")
	said(t, h.Chat("doberman", "!enter", "broadcaster")) // told once

	h.Clock.Advance(5 * time.Minute)
	said(t, h.Said(), "Entries for Winterpaw keychain are closed: 3 entered. A mod can !giveaway draw now.")
	said(t, h.Chat("bat", "!enter")) // too late

	e := export(t, h)
	tickets := map[string]int{}
	for _, en := range e.Entries {
		tickets[en.Login] = en.Tickets
	}
	if !maps(tickets, map[string]int{"fox": 1, "owl": 2, "elk": 1}) {
		t.Fatalf("tickets %v", tickets)
	}

	win := saidPrefix(t, h.Chat("mink", "!giveaway draw", "luna-mod"), "🎉 ")
	winner := strings.Fields(win)[1]
	if out, err := h.CLI("verify", nil); err != nil || !strings.HasPrefix(out.(string), "OK") {
		t.Fatalf("verify: %v %v", out, err)
	}
	said(t, h.Chat(winner, "it's me!"), fmt.Sprintf("@%s claimed Winterpaw keychain! 🎉", winner))
	h.Clock.Advance(3 * time.Minute)
	said(t, h.Said()) // claimed, so no "didn't claim"

	second := saidPrefix(t, h.Chat("mink", "!giveaway reroll", "luna-mod"), "🎉 ")
	if w2 := strings.Fields(second)[1]; w2 == winner {
		t.Fatal("reroll picked the same winner")
	}
	h.Clock.Advance(2 * time.Minute)
	if got := h.Said(); len(got) != 1 || !strings.HasSuffix(got[0], "didn't claim in time. A mod can !giveaway reroll.") {
		t.Fatalf("no claim timeout: %q", got)
	}
	if out, err := h.CLI("verify", nil); err != nil || !strings.Contains(out.(string), "all 2 draws") {
		t.Fatalf("verify after reroll: %v %v", out, err)
	}
	ws, _ := h.CLI("winners", nil)
	if rows := ws.([]any); len(rows) != 2 || rows[1].(map[string]any)["claimed"] != true ||
		rows[1].(map[string]any)["prize"] != "Winterpaw keychain" {
		t.Fatalf("winners %v", ws)
	}
}

func maps(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestVerifyCatchesTampering(t *testing.T) {
	h := live(t, nil)
	h.Chat("mink", "!giveaway start Prize", "luna-mod")
	for _, u := range []string{"a", "b", "c", "d"} {
		h.Chat(u, "!enter")
	}
	h.Chat("mink", "!giveaway draw", "luna-mod")
	e := export(t, h)
	if _, err := Verify(e); err != nil {
		t.Fatal(err)
	}
	bad := *e
	bad.Giveaway.Draws = slices.Clone(e.Giveaway.Draws)
	for _, en := range e.Entries {
		if en.UserID != e.Giveaway.Draws[0].UserID {
			bad.Giveaway.Draws[0].UserID, bad.Giveaway.Draws[0].Login = en.UserID, en.Login
			break
		}
	}
	if _, err := Verify(&bad); err == nil {
		t.Fatal("a swapped winner verified")
	}
	bad = *e
	bad.Giveaway.Seed = strings.Repeat("00", 32)
	if _, err := Verify(&bad); err == nil || !strings.Contains(err.Error(), "commitment") {
		t.Fatalf("a different seed verified: %v", err)
	}
}

// Over many draws, each entry wins in proportion to its tickets.
func TestPickIsWeighted(t *testing.T) {
	es := []Entrant{{UserID: "a", Tickets: 1}, {UserID: "b", Tickets: 2}, {UserID: "c", Tickets: 1}}
	wins := map[string]int{}
	for n := 1; n <= 8000; n++ {
		w, _ := Pick([]byte("seed"), n, es, nil)
		wins[w.UserID]++
	}
	for id, want := range map[string]int{"a": 2000, "b": 4000, "c": 2000} {
		if d := wins[id] - want; d < -200 || d > 200 {
			t.Errorf("%s won %d times, want about %d", id, wins[id], want)
		}
	}
	if _, ok := Pick([]byte("seed"), 1, es, []string{"a", "b", "c"}); ok {
		t.Error("picked from an empty pool")
	}
}

func TestEntryRules(t *testing.T) {
	h := live(t, map[string]any{"exclude_mods": true, "min_account_days": 30, "winner_cooldown_days": 7})
	h.Twitch.Follows["id-fox"] = h.Clock.Now().Add(-10 * 24 * time.Hour)
	h.Twitch.Follows["id-owl"] = h.Clock.Now().Add(-2 * 24 * time.Hour)
	for _, u := range []string{"fox", "owl", "elk", "hare"} {
		h.Twitch.Users[u] = &luna.TwitchUser{ID: "id-" + u, Login: u, CreatedAt: h.Clock.Now().AddDate(-1, 0, 0)}
	}
	h.Twitch.Users["elk"].CreatedAt = h.Clock.Now().Add(-time.Hour)

	h.Chat("mink", "!giveaway start Prize min_follow=7d", "luna-mod")
	said(t, h.Chat("fox", "!enter"))
	said(t, h.Chat("owl", "!enter"), "This giveaway needs you to have followed for 7 days.")
	said(t, h.Chat("bat", "!enter"), "This giveaway is for followers only.")
	said(t, h.Chat("mink", "!enter", "moderator"), "Mods can't enter giveaways here.")
	h.Twitch.Follows["id-elk"] = h.Clock.Now().Add(-30 * 24 * time.Hour)
	said(t, h.Chat("elk", "!enter"), "Your account is too new to enter.")

	saidPrefix(t, h.Chat("mink", "!giveaway draw", "luna-mod"), "🎉 fox wins")
	h.Chat("mink", "!giveaway start Another", "luna-mod")
	said(t, h.Chat("fox", "!enter"), "You won recently, so sit this one out. 💜")
	said(t, h.Chat("hare", "!enter")) // no follow needed for this one
	said(t, h.Chat("mink", "!giveaway draw", "luna-mod"), "🎉 hare wins Another! Type anything in chat within 2m0s to claim. Seed: "+export(t, h).Giveaway.Seed)
	said(t, h.Chat("mink", "!giveaway draw", "luna-mod"), "Already drawn: use !giveaway reroll to pick someone else.")
}

func TestTicketGiveaway(t *testing.T) {
	h := live(t, nil)
	h.Chat("mink", "!giveaway start Prize tickets=100 max=5", "luna-mod")
	said(t, h.Chat("fox", "!enter 2"), "Ticket giveaways need the Points plugin running: luna capsule install points")

	bal := map[string]int64{"id-fox": 1000, "id-owl": 150}
	h.Link("luna.points:spend", func(a luna.Args) (any, error) {
		if bal[a.Str("user_id")] < a.Int("amount") {
			return nil, errors.New("not enough points: you have " + fmt.Sprint(bal[a.Str("user_id")]))
		}
		bal[a.Str("user_id")] -= a.Int("amount")
		return map[string]any{"balance": bal[a.Str("user_id")]}, nil
	})
	flaky := true // the first refund to owl fails, as if Points were restarting
	h.Link("luna.points:add", func(a luna.Args) (any, error) {
		if a.Str("user_id") == "id-owl" && flaky {
			flaky = false
			return nil, luna.ErrLinkUnavailable
		}
		bal[a.Str("user_id")] += a.Int("amount")
		return map[string]any{"balance": bal[a.Str("user_id")]}, nil
	})
	said(t, h.Chat("fox", "!enter 2")) // bought quietly
	said(t, h.Chat("fox", "!enter 9")) // capped at max
	said(t, h.Chat("fox", "!enter"), "You already have the maximum of 5 tickets.")
	said(t, h.Chat("owl", "!enter 2"), "not enough points: you have 150")
	said(t, h.Chat("owl", "!enter"))
	if bal["id-fox"] != 500 || bal["id-owl"] != 50 {
		t.Fatalf("balances %v", bal)
	}
	if got := h.Chat("mink", "!giveaway cancel", "luna-mod"); len(got) != 1 || !strings.HasPrefix(got[0], "refunds stopped after 1") {
		t.Fatalf("cancel with a failing refund: %q", got)
	}
	said(t, h.Chat("mink", "!giveaway cancel", "luna-mod"), "The Prize giveaway was cancelled. Tickets refunded to 1 people.")
	if bal["id-fox"] != 1000 || bal["id-owl"] != 150 { // fox refunded once, not twice
		t.Fatalf("not refunded: %v", bal)
	}
	said(t, h.Chat("mink", "!giveaway draw", "luna-mod"), "There's no giveaway to draw.")
}

// An entry sent before the deadline counts even when chat is backed up and it's handled after.
func TestLateHandledEntry(t *testing.T) {
	b := New()
	h := lunatest.New(t, b, ".", nil)
	h.Emit("twitch.stream.online", nil)
	h.Chat("mink", "!giveaway start Prize duration=5m", "luna-mod")
	sent := h.Clock.Now().Add(4 * time.Minute)
	h.Clock.Advance(5 * time.Minute) // closed by the timer
	h.Said()
	entry := func(login string, at time.Time) {
		raw, _ := json.Marshal(map[string]any{"user": map[string]any{"id": "id-" + login, "login": login, "rank": "viewer"},
			"text": "!enter", "message_id": "m-" + login})
		b.TestDispatch(&luna.Event{ID: "e-" + login, Type: "twitch.chat.message", TS: at, Data: raw})
	}
	entry("fox", sent)                           // typed in time, handled late
	entry("owl", h.Clock.Now().Add(time.Second)) // typed after the close
	if es := export(t, h).Entries; len(es) != 1 || es[0].Login != "fox" {
		t.Fatalf("entries %+v", es)
	}
	h.Chat("mink", "!giveaway draw", "luna-mod")
	entry("elk", sent) // once drawn, the list is frozen
	if es := export(t, h).Entries; len(es) != 1 {
		t.Fatalf("entry after the draw: %+v", es)
	}
}
