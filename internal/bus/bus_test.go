package bus

import (
	"testing"

	"luna/internal/event"
)

func TestSlowSubscriberDoesNotBlock(t *testing.T) {
	b := New()
	slow := b.Subscribe(1)
	hr := b.Subscribe(10, "hr.update")
	for range 5 {
		ev, _ := event.New("hr.update", "hr", true, map[string]int{"bpm": 90})
		b.Publish(ev)
	}
	ev, _ := event.New("twitch.channel.follow", "twitch", true, nil)
	b.Publish(ev)

	if got := slow.Lagged(); got != 5 {
		t.Fatalf("slow lagged = %d, want 5", got)
	}
	if got := len(hr.C); got != 5 {
		t.Fatalf("hr queued = %d, want 5 (follow must be filtered out)", got)
	}
	hr.Close()
	b.Publish(ev) // must not panic on a closed subscriber
}

func BenchmarkPublish(b *testing.B) {
	bus := New()
	for range 8 {
		s := bus.Subscribe(4096, "twitch.chat.message")
		go func() {
			for range s.C {
			}
		}()
	}
	ev, _ := event.New("twitch.chat.message", "twitch", true, map[string]string{"text": "hi"})
	for b.Loop() {
		bus.Publish(ev)
	}
}
