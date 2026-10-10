package main

import (
	"encoding/json"
	"testing"
)

func TestSimEvent(t *testing.T) {
	ev, err := simEvent([]string{"sub", "--user", "test", "--tier", "2"})
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		User struct{ ID, Login string }
		Tier int
	}
	json.Unmarshal(ev.Data, &d)
	if ev.Type != "twitch.channel.subscribe" || !ev.Simulated || d.User.ID != "sim-test" || d.Tier != 2 {
		t.Fatalf("bad sub event: %+v %s", ev, ev.Data)
	}
	for _, bad := range [][]string{{"hr", "999"}, {"sub", "--tier", "4"}, {"nope"}, {"redeem"}, {}} {
		if _, err := simEvent(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
