package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"luna/internal/event"
	"luna/internal/store"
)

const sessionGap = 30 * time.Minute

// eventsCmd serves `luna events sessions | replay <n> [--speed 4]`.
func eventsCmd(ctx context.Context, args []string, st *store.Store, publish func(event.Event)) (string, error) {
	if len(args) == 0 {
		args = []string{"sessions"}
	}
	sessions, err := st.Sessions(sessionGap)
	if err != nil {
		return "", err
	}
	switch args[0] {
	case "sessions":
		if len(sessions) == 0 {
			return "no recorded sessions yet", nil
		}
		var b strings.Builder
		for i, s := range sessions {
			fmt.Fprintf(&b, "%3d  %s  %-9s %d events\n", i+1, s.Start.Local().Format("2006-01-02 15:04"),
				s.End.Sub(s.Start).Round(time.Minute), s.Events)
		}
		return strings.TrimRight(b.String(), "\n"), nil
	case "replay":
		fs := flag.NewFlagSet("replay", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		speed := fs.Float64("speed", 1, "")
		if err := fs.Parse(args[1:]); err != nil {
			return "", err
		}
		n, err := strconv.Atoi(fs.Arg(0))
		if fs.NArg() > 0 { // flags may also follow the session number
			if err := fs.Parse(fs.Args()[1:]); err != nil {
				return "", err
			}
		}
		if err != nil || n < 1 || n > len(sessions) || fs.NArg() != 0 {
			return "", fmt.Errorf("usage: luna events replay <1-%d> [--speed N] (see luna events sessions)", len(sessions))
		}
		if *speed <= 0 || *speed > 1000 {
			return "", errors.New("--speed must be between 0 and 1000")
		}
		s := sessions[n-1]
		evs, err := st.RealEvents(s.Start, s.End)
		if err != nil {
			return "", err
		}
		go replay(ctx, evs, *speed, publish)
		return fmt.Sprintf("replaying %d events over %s", len(evs), (time.Duration(float64(s.End.Sub(s.Start)) / (*speed))).Round(time.Second)), nil
	}
	return "", errors.New("usage: luna events sessions | replay <n> [--speed N]")
}

// replay republishes evs with their original spacing divided by speed, as new simulated events.
// ponytail: no stop command; a replay runs until done or the daemon stops.
func replay(ctx context.Context, evs []event.Event, speed float64, publish func(event.Event)) {
	for i, ev := range evs {
		if i > 0 {
			select {
			case <-time.After(time.Duration(float64(ev.TS.Sub(evs[i-1].TS)) / speed)):
			case <-ctx.Done():
				return
			}
		}
		sim, err := event.New(ev.Type, ev.Source, true, ev.Data)
		if err != nil {
			continue
		}
		publish(sim)
	}
	slog.Info("replay finished", "events", len(evs))
}
