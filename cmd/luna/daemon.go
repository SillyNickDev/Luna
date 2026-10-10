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
	"luna/internal/event"
	"luna/internal/guard"
	"luna/internal/secrets"
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
	startTwitch(ctx, p, cfg.Get().Twitch.ClientID, g, b.Publish)

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

// startTwitch connects the broadcaster account and, if logged in, the bot account,
// which then reads and sends chat. Without a broadcaster login Luna runs offline.
// ponytail: client ID and tokens are read once; changing them needs a restart.
func startTwitch(ctx context.Context, p paths, clientID string, g *guard.Guard, publish func(event.Event)) {
	store := secrets.Open(p.data)
	if !store.UsesKeyring() {
		slog.Warn("no OS keyring; twitch tokens are in encrypted files under the data dir")
	}
	bc, err := twitch.NewClient(clientID, store, "broadcaster")
	if err != nil {
		slog.Warn("running without twitch", "err", err)
		return
	}
	run := func(c *twitch.Client, subs []twitch.Sub) {
		go func() {
			if err := c.Run(ctx, subs, publish); err != nil {
				slog.Error("twitch stopped", "err", err)
			}
		}()
	}
	b := bc.UserID()
	chatter := bc
	if bot, err := twitch.NewClient(clientID, store, "bot"); err == nil {
		chatter = bot
		run(bot, []twitch.Sub{twitch.ChatSub(b, bot.UserID())})
		run(bc, twitch.ChannelSubs(b))
	} else {
		slog.Info("no bot account; chatting as the broadcaster", "hint", "luna auth login bot")
		run(bc, append(twitch.ChannelSubs(b), twitch.ChatSub(b, b)))
	}
	g.Live = func(ctx context.Context, text string) error { return chatter.SendChat(ctx, b, text) }
}
