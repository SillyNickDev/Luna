package twitch

import (
	"encoding/json"
	"testing"
)

func TestToEvent(t *testing.T) {
	chat := `{"chatter_user_id":"42","chatter_user_login":"viewer","message_id":"m1",
		"message":{"text":"hi /ban x","fragments":[]},"badges":[{"set_id":"subscriber"},{"set_id":"moderator"},{"set_id":"glhf-pledge"}]}`
	ev, err := toEvent("channel.chat.message", json.RawMessage(chat))
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		User struct {
			ID    string
			Roles []string
		}
		Text string
	}
	json.Unmarshal(ev.Data, &d)
	if ev.Type != "twitch.chat.message" || ev.Simulated || d.User.ID != "42" || d.Text != "hi /ban x" ||
		len(d.User.Roles) != 2 || d.User.Roles[0] != "sub" {
		t.Fatalf("chat: %s %s", ev.Type, ev.Data)
	}

	cheer := `{"is_anonymous":true,"user_id":null,"user_login":null,"message":"pog","bits":100}`
	if ev, err := toEvent("channel.cheer", json.RawMessage(cheer)); err != nil || ev.Type != "twitch.channel.cheer" {
		t.Fatalf("cheer: %v %s", err, ev.Data)
	}
	sub := `{"user_id":"7","user_login":"s","tier":"2000","is_gift":false}`
	if ev, _ := toEvent("channel.subscribe", json.RawMessage(sub)); string(ev.Data) != `{"gift":false,"tier":2,"user":{"id":"7","login":"s","name":"","roles":[]}}` {
		t.Fatalf("sub: %s", ev.Data)
	}
	gift := `{"user_id":"7","user_login":"s","user_name":"S","tier":"1000","total":50,"is_anonymous":false}`
	if ev, _ := toEvent("channel.subscription.gift", json.RawMessage(gift)); ev.Type != "twitch.channel.gift" ||
		string(ev.Data) != `{"anonymous":false,"count":50,"tier":1,"user":{"id":"7","login":"s","name":"S","roles":[]}}` {
		t.Fatalf("gift: %s", ev.Data)
	}
	if _, err := toEvent("channel.unknown", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unknown type accepted")
	}
}

func TestDedupe(t *testing.T) {
	d := newDedupe(2)
	if !d.add("a") || d.add("a") || !d.add("b") || !d.add("c") || !d.add("a") || d.add("c") {
		t.Fatal("dedupe window wrong")
	}
}
