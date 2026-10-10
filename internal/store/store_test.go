package store

import (
	"path/filepath"
	"testing"

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
