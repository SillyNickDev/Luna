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
	close(c)
	s.Log(c)

	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM events WHERE type = ?`, ev.Type).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("logged %d events, want 1", n)
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
