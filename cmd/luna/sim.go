package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"luna/internal/event"
)

var simList = []struct{ name, typ, usage string }{
	{"chat", "twitch.chat.message", `[--user x] [--role sub,vip] "text"`},
	{"follow", "twitch.channel.follow", "[--user x]"},
	{"sub", "twitch.channel.subscribe", "[--user x] [--tier 1|2|3]"},
	{"cheer", "twitch.channel.cheer", "[--user x] [--bits N]"},
	{"raid", "twitch.channel.raid", "[--user x] [--viewers N]"},
	{"redeem", "twitch.channel.redeem", `[--user x] "Reward" [input]`},
	{"hr", "hr.update", "<bpm>"},
}

// simEvent turns `luna sim` arguments into a simulated event.
func simEvent(args []string) (event.Event, error) {
	if len(args) == 0 {
		return event.Event{}, errors.New("usage: luna sim <event> [flags] (see luna sim --list)")
	}
	name, typ := args[0], ""
	for _, s := range simList {
		if s.name == name {
			typ = s.typ
		}
	}
	if typ == "" {
		return event.Event{}, fmt.Errorf("unknown sim event %q (see luna sim --list)", name)
	}

	fs := flag.NewFlagSet("sim "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	login := fs.String("user", "testviewer", "")
	role := fs.String("role", "", "")
	tier := fs.Int("tier", 1, "")
	bits := fs.Int("bits", 100, "")
	viewers := fs.Int("viewers", 10, "")
	if err := fs.Parse(args[1:]); err != nil {
		return event.Event{}, err
	}
	pos := fs.Args()
	roles := []string{}
	if *role != "" {
		roles = strings.Split(*role, ",")
	}
	user := map[string]any{"id": "sim-" + *login, "login": *login, "roles": roles}

	var data map[string]any
	switch name {
	case "chat":
		text := strings.Join(pos, " ")
		if text == "" || utf8.RuneCountInString(text) > 500 {
			return event.Event{}, errors.New("chat text must be 1-500 characters")
		}
		data = map[string]any{"user": user, "text": text}
	case "follow":
		data = map[string]any{"user": user}
	case "sub":
		if *tier < 1 || *tier > 3 {
			return event.Event{}, errors.New("--tier must be 1, 2 or 3")
		}
		data = map[string]any{"user": user, "tier": *tier}
	case "cheer":
		if *bits < 1 {
			return event.Event{}, errors.New("--bits must be at least 1")
		}
		data = map[string]any{"user": user, "bits": *bits}
	case "raid":
		if *viewers < 0 {
			return event.Event{}, errors.New("--viewers can't be negative")
		}
		data = map[string]any{"user": user, "viewers": *viewers}
	case "redeem":
		if len(pos) == 0 {
			return event.Event{}, errors.New(`usage: luna sim redeem "Reward" [input]`)
		}
		data = map[string]any{"user": user, "reward": pos[0], "input": strings.Join(pos[1:], " ")}
	case "hr":
		bpm, err := strconv.Atoi(strings.Join(pos, ""))
		if err != nil || bpm < 20 || bpm > 250 {
			return event.Event{}, errors.New("usage: luna sim hr <bpm 20-250>")
		}
		return event.New(typ, "hr", true, map[string]any{"bpm": bpm})
	}
	return event.New(typ, "twitch", true, data)
}
