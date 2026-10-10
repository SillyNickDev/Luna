package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"luna/internal/event"
	"luna/internal/perms"
	"luna/internal/store"
)

func TestWithRanks(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "luna.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ranks, _ := loadRanks(st)
	var got event.Event
	publish := withRanks(ranks, st, func(ev event.Event) { got = ev })
	chat := func(simulated bool, id string, roles ...string) string {
		ev, _ := event.New("twitch.chat.message", "twitch", simulated, map[string]any{
			"user": map[string]any{"id": id, "login": "u" + id, "roles": roles}, "text": "hi"})
		publish(ev)
		var d struct{ User struct{ Rank string } }
		json.Unmarshal(got.Data, &d)
		return d.User.Rank
	}

	if r := chat(true, "1", "lead_moderator"); r != "moderator" {
		t.Fatalf("simulated lead mod became %s", r)
	}
	if r := chat(false, "1", "lead_moderator"); r != "head-mod" {
		t.Fatalf("lead mod = %s, want head-mod", r)
	}
	if rows, _ := st.Ranks(); len(rows) != 1 || rows[0].Rank != "head-mod" {
		t.Fatalf("head mod not saved: %+v", rows)
	}
	if r := chat(false, "2", "sub"); r != "viewer" {
		t.Fatalf("viewer = %s", r)
	}

	ev, _ := event.New("twitch.channel.moderator.remove", "twitch", false, map[string]any{"user": map[string]any{"id": "1"}})
	publish(ev)
	if ranks.Get("1") != perms.Viewer {
		t.Fatal("unmod didn't drop the kept rank")
	}
}
