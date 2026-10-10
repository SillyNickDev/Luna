package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"luna/internal/bus"
	"luna/internal/config"
	"luna/internal/guard"
	"luna/internal/store"
	"luna/internal/twitch"
)

func runDaemon(p paths) error {
	if os.Geteuid() == 0 {
		return errors.New("refusing to run as root")
	}
	cfg, err := config.NewHolder(p.config)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	debug.SetMemoryLimit(int64(cfg.Get().MemoryLimitMiB) << 20)

	if err := os.MkdirAll(p.data, 0o700); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(p.data, "luna.db"))
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer st.Close()

	b := bus.New()
	logSub := b.Subscribe(4096)
	logged := make(chan struct{})
	go func() { st.Log(logSub.C); close(logged) }()
	printSub := b.Subscribe(256)
	go func() {
		for ev := range printSub.C {
			slog.Info("event", "type", ev.Type, "simulated", ev.Simulated, "data", string(ev.Data))
		}
	}()

	ln, err := listen(p.sock)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); ln.Close() }()

	g := guard.New(cfg.Get)
	// ponytail: client ID and token are read once; changing them needs a restart.
	if tw, err := twitch.NewClient(cfg.Get().Twitch.ClientID, p.tokens); err != nil {
		slog.Warn("running without twitch", "err", err)
	} else {
		g.Live = tw.SendChat
		go func() {
			if err := tw.Run(ctx, b.Publish); err != nil {
				slog.Error("twitch stopped", "err", err)
			}
		}()
	}

	slog.Info("luna running", "config", p.config, "data", p.data)
	serve(ln, func(req request) (string, error) {
		switch req.Cmd {
		case "sim":
			ev, err := simEvent(req.Args)
			if err != nil {
				return "", err
			}
			b.Publish(ev)
			return fmt.Sprintf("fired %s (%s)", ev.Type, ev.ID), nil
		case "chat":
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := g.Chat(cctx, strings.Join(req.Args, " "), false); err != nil {
				return "", err
			}
			return "sent", nil
		case "reload":
			if err := cfg.Reload(); err != nil {
				return "", fmt.Errorf("kept the last good config: %w", err)
			}
			debug.SetMemoryLimit(int64(cfg.Get().MemoryLimitMiB) << 20)
			return "config reloaded", nil
		}
		return "", fmt.Errorf("unknown command %q", req.Cmd)
	})

	slog.Info("shutting down")
	printSub.Close()
	logSub.Close()
	<-logged // flush the event log before the database closes
	return nil
}
