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
	"luna/internal/plugin"
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

	ranks, err := loadRanks(st)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	b := bus.New()
	publish := withRanks(ranks, st, b.Publish)
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

	onReload := func(err error) {
		if err != nil {
			slog.Error("config not reloaded; keeping the last good one", "err", err)
			return
		}
		debug.SetMemoryLimit(int64(cfg.Get().MemoryLimitMiB) << 20)
		slog.Info("config reloaded")
	}
	if err := cfg.Watch(ctx, onReload); err != nil {
		slog.Warn("config file watching off; use luna reload", "err", err)
	}

	g := guard.New(cfg.Get)
	tw := startTwitch(ctx, p, cfg.Get().Twitch.ClientID, g, publish)
	host := &plugin.Host{Bus: b, Publish: publish, Store: st, Guard: g}
	caps, err := startCapsules(p, st, publish, host, cfg.Get)
	if err != nil {
		return fmt.Errorf("capsules: %w", err)
	}

	slog.Info("luna running", "config", p.config, "data", p.data)
	serve(ln, func(req request) (string, error) {
		switch req.Cmd {
		case "sim":
			ev, err := simEvent(req.Args)
			if err != nil {
				return "", err
			}
			publish(ev)
			return fmt.Sprintf("fired %s (%s)", ev.Type, ev.ID), nil
		case "chat":
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := g.Chat(cctx, strings.Join(req.Args, " "), false); err != nil {
				return "", err
			}
			return "sent", nil
		case "events":
			return eventsCmd(ctx, req.Args, st, publish)
		case "perms":
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			return permsCmd(pctx, req.Args, ranks, st, tw, policyFrom(cfg.Get()))
		case "capsule":
			return capsuleCmd(p, req.Args, caps, st)
		case "reload":
			err := cfg.Reload()
			onReload(err)
			if err != nil {
				return "", fmt.Errorf("kept the last good config: %w", err)
			}
			return "config reloaded", nil
		}
		return "", fmt.Errorf("unknown command %q", req.Cmd)
	})

	slog.Info("shutting down")
	caps.StopAll()
	printSub.Close()
	logSub.Close()
	<-logged // flush the event log before the database closes
	return nil
}

// startTwitch connects the broadcaster account and, if logged in, the bot account,
// which then reads and sends chat. Without a broadcaster login Luna runs offline.
// ponytail: client ID and tokens are read once; changing them needs a restart.
func startTwitch(ctx context.Context, p paths, clientID string, g *guard.Guard, publish func(event.Event)) *twitch.Client {
	store := secrets.Open(p.data)
	if !store.UsesKeyring() {
		slog.Warn("OS keyring unreachable from this session; using encrypted token files instead. " +
			"Logins saved to the keyring from a desktop session aren't visible here: log in from this session or start luna from the desktop")
	}
	bc, err := twitch.NewClient(clientID, store, "broadcaster")
	if err != nil {
		slog.Warn("running without twitch", "err", err)
		return nil
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
	return bc
}
