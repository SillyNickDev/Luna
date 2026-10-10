package perms

import "testing"

// Rows from the "What override means in practice" table, plus locks and own actions.
func TestCanOverride(t *testing.T) {
	p := DefaultPolicy()
	cases := []struct {
		name string
		id   string
		r    Rank
		a    Action
		ok   bool
	}{
		{"mod unbans head mod's ban", "m1", Moderator, Action{ActorID: "h1", ActorRank: HeadMod}, false},
		{"luna mod undoes mod's timeout", "l1", LunaMod, Action{ActorID: "m1", ActorRank: Moderator}, true},
		{"mods disagree, same-rank off", "m2", Moderator, Action{ActorID: "m1", ActorRank: Moderator}, false},
		{"luna mods, same-rank on", "l2", LunaMod, Action{ActorID: "l1", ActorRank: LunaMod}, true},
		{"broadcaster reverses a lock", "b", Broadcaster, Action{ActorID: "h1", ActorRank: HeadMod, Locked: true, LockRank: Broadcaster}, true},
		{"own action", "m1", Moderator, Action{ActorID: "m1", ActorRank: Moderator}, true},
		{"own action locked higher", "m1", Moderator, Action{ActorID: "m1", ActorRank: Moderator, Locked: true, LockRank: HeadMod}, false},
	}
	for _, c := range cases {
		if err := p.CanOverride(c.id, c.r, c.a); (err == nil) != c.ok {
			t.Errorf("%s: got err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestCan(t *testing.T) {
	p := DefaultPolicy()
	p.UserGrants = map[string]map[string]bool{"v1": {"mod.ban": true, "perms.grant": true, "giveaway.run": true}}
	switch {
	case p.Can("v1", Viewer, "mod.ban"):
		t.Error("viewer got a Twitch mod power")
	case p.Can("v1", Viewer, "perms.grant"):
		t.Error("broadcaster-only capability granted to a viewer")
	case !p.Can("v1", Viewer, "giveaway.run"):
		t.Error("user grant ignored")
	case !p.Can("l1", LunaMod, "giveaway.run"):
		t.Error("luna mod default missing")
	case p.Can("x", HeadMod, "auth.login"):
		t.Error("auth.* below broadcaster")
	}
	p.Lockdown = true
	if p.Can("l1", LunaMod, "luna.chat_cli") || p.Can("v1", Viewer, "giveaway.run") {
		t.Error("lockdown didn't freeze luna mods / user grants")
	}
}

func TestResolve(t *testing.T) {
	r := NewRanks(map[string]Rank{"h": HeadMod, "l": LunaMod})
	cases := []struct {
		id    string
		roles []string
		want  Rank
	}{
		{"b", []string{"broadcaster", "sub"}, Broadcaster},
		{"h", []string{"moderator"}, HeadMod},
		{"h", []string{"sub"}, Viewer}, // unmodded on Twitch: kept rank doesn't count
		{"l", []string{"moderator", "vip"}, LunaMod},
		{"m", []string{"moderator"}, Moderator},
		{"v", nil, Viewer},
	}
	for _, c := range cases {
		if got := r.Resolve(c.id, c.roles); got != c.want {
			t.Errorf("Resolve(%s, %v) = %s, want %s", c.id, c.roles, got, c.want)
		}
	}
}
