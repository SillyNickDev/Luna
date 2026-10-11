package store

import (
	"path/filepath"
	"testing"
	"time"

	"luna/internal/event"
)

func TestLogFlushesOnClose(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "luna.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	c := make(chan event.Event, 10)
	ev, _ := event.New("twitch.channel.raid", "twitch", true, map[string]int{"viewers": 40})
	c <- ev
	c <- ev // duplicate ID is ignored
	param, _ := event.New("osc.param", "osc", false, map[string]any{"name": "JawOpen", "value": 0.4})
	c <- param // live avatar state isn't history
	close(c)
	s.Log(c)

	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM events WHERE type = ?`, ev.Type).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("logged %d events, want 1", n)
	}
	if s.db.QueryRow(`SELECT count(*) FROM events`).Scan(&n); n != 1 {
		t.Fatalf("logged %d events in all, want just the raid", n)
	}
}

func TestSessions(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "luna.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := time.Date(2026, 10, 10, 18, 0, 0, 0, time.UTC)
	var evs []event.Event
	for i, min := range []int{0, 10, 20, 120, 125} { // two streams, split by a 100-minute gap
		ev, _ := event.New("twitch.chat.message", "twitch", false, map[string]int{"i": i})
		ev.TS = base.Add(time.Duration(min) * time.Minute)
		evs = append(evs, ev)
	}
	sim, _ := event.New("hr.update", "hr", true, nil)
	if err := s.write(append(evs, sim)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Sessions(30 * time.Minute)
	if err != nil || len(got) != 2 || got[0].Events != 2 || got[1].Events != 3 {
		t.Fatalf("sessions = %+v, %v", got, err)
	}
	real, err := s.RealEvents(got[1].Start, got[1].End)
	if err != nil || len(real) != 3 || !real[0].TS.Equal(base) {
		t.Fatalf("real events = %d, %v", len(real), err)
	}
}

func TestPluginStore(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "luna.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n, err := s.PluginIncr("a", "deaths", 2); err != nil || n != 2 {
		t.Fatalf("incr: %d %v", n, err)
	}
	if n, _ := s.PluginIncr("a", "deaths", 1); n != 3 {
		t.Fatalf("incr again: %d", n)
	}
	s.PluginSet("a", "cool", []byte(`1`), time.Millisecond)
	s.PluginSet("b", "deaths", []byte(`"other plugin"`), 0)
	time.Sleep(5 * time.Millisecond)
	if v, _ := s.PluginGet("a", "cool"); v != nil {
		t.Fatalf("expired value still there: %s", v)
	}
	if keys, _ := s.PluginKeys("a", "de"); len(keys) != 1 || keys[0] != "deaths" {
		t.Fatalf("keys: %v", keys)
	}
	if v, _ := s.PluginGet("a", "deaths"); string(v) != "3" {
		t.Fatalf("plugins see each other's data: %s", v)
	}
	if _, err := s.PluginIncr("b", "deaths", 1); err == nil {
		t.Fatal("incr of a string value accepted")
	}
	if err := s.PluginSet("a", "big", make([]byte, MaxValue+1), 0); err == nil {
		t.Fatal("oversized value accepted")
	}
}

func TestViewers(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t0 := time.Unix(1_800_000_000, 0)
	if prev, err := s.SeeChatter("1", "fox", t0, 1); prev != nil || err != nil {
		t.Fatalf("first sight: %v %v", prev, err)
	}
	s.SeeChatter("1", "fox", t0.Add(5*time.Minute), 1)  // counts: 5 min watched
	s.SeeChatter("1", "fox", t0.Add(30*time.Minute), 1) // gap over 10 min: not counted
	s.SeeChatter("1", "fox", t0.Add(48*time.Hour), 2)   // next stream: streak 2
	s.SeeChatter("1", "fox", t0.Add(96*time.Hour), 4)   // missed stream 3: streak resets
	v, _ := s.GetViewer("FOX")
	if v == nil || v.Messages != 5 || v.WatchS != 300 || v.Streams != 3 || v.Streak != 1 || !v.FirstSeen.Equal(t0) {
		t.Fatalf("got %+v", v)
	}
	if err := s.PluginBatch("p", []KV{{"a", []byte("1"), 0}, {"b", []byte("2"), 0}}, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if keys, _ := s.PluginKeys("p", ""); len(keys) != 1 || keys[0] != "b" {
		t.Fatalf("batch: %v", keys)
	}
	if err := s.PluginBatch("p", []KV{{"c", []byte("3"), 0}, {"", nil, 0}}, nil); err == nil {
		t.Fatal("a bad key in a batch was accepted")
	}
	if v, _ := s.PluginGet("p", "c"); v != nil {
		t.Fatal("a failed batch wrote part of itself")
	}
}
