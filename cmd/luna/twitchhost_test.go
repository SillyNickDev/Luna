package main

import (
	"testing"
	"time"
)

// Chatting as the broadcaster, only Luna's own recent messages are skipped as commands.
func TestEchoes(t *testing.T) {
	var e echoes
	now := time.Now()
	e.add("!discord  is in the panel", now)
	if !e.take("!discord is in the panel", now.Add(time.Second)) {
		t.Fatal("Luna's own message wasn't recognised")
	}
	if e.take("!discord is in the panel", now.Add(2*time.Second)) {
		t.Fatal("the broadcaster typing the same text again was skipped")
	}
	if e.take("!game Pacific Drive", now) {
		t.Fatal("a typed command was taken for Luna's")
	}
	e.add("!so fox", now)
	if e.take("!so fox", now.Add(31*time.Second)) {
		t.Fatal("an old message still counted as Luna's")
	}
}
