package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"luna/internal/bus"
	"luna/internal/capsule"
	"luna/internal/config"
	"luna/internal/event"
	"luna/internal/guard"
	"luna/internal/perms"
	"luna/internal/plugin"
	"luna/internal/secrets"
	"luna/internal/store"
	"luna/internal/twitch"
)

func runDaemon(p paths) error {
	if os.Geteuid() == 0 {
		return errors.New("refusing to run as root")
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, daemonLog), nil)))
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
	vt := &viewerTracker{st: st}
	publish := withRanks(ranks, st, vt.wrap(b.Publish))
	logSub := b.Subscribe(4096)
	logged := make(chan struct{})
	go func() { st.Log(logSub.C); close(logged) }()
	printSub := b.Subscribe(256) // recent events for luna events tail; each one at debug level in the log
	go func() {
		for ev := range printSub.C {
			if line, err := json.Marshal(ev); err == nil {
				recentEvents.Write(append(line, '\n'))
			}
			slog.Debug("event", "type", ev.Type, "simulated", ev.Simulated, "data", string(ev.Data))
		}
	}()

	ln, err := listen(p.sock)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); ln.Close() }()

	var caps *capsule.Runtime // set below; a reload before then has no plugins to restart
	lastPlugins := cfg.Get().Plugins
	onReload := func(err error) {
		if err != nil {
			slog.Error("config not reloaded; keeping the last good one", "err", err)
			return
		}
		debug.SetMemoryLimit(int64(cfg.Get().MemoryLimitMiB) << 20)
		slog.Info("config reloaded")
		restartChangedPlugins(caps, lastPlugins, cfg.Get().Plugins)
		lastPlugins = cfg.Get().Plugins
	}
	if err := cfg.Watch(ctx, onReload); err != nil {
		slog.Warn("config file watching off; use luna reload", "err", err)
	}

	g := guard.New(cfg.Get)
	tw, th := startTwitch(ctx, p, cfg.Get().Twitch.ClientID, g, publish)
	host := &plugin.Host{Bus: b, Publish: publish, Store: st, Guard: g, HR: &plugin.HR{}}
	hrSub := b.Subscribe(64, "hr.update")
	go feedHR(hrSub, host.HR)
	if sh := startSpotify(cfg.Get(), secrets.Open(p.data)); sh != nil {
		host.Spotify = sh // only when connected: a nil *spotifyHost would make a non-nil interface
	}
	if dh := startDeepL(secrets.Open(p.data)); dh != nil {
		host.Translator = dh
	}
	oc := startOSC(ctx, cfg.Get(), publish)
	if oc != nil {
		host.OSC = oc
	}
	if th != nil {
		host.Twitch = th
		go func() { // a stream already running when Luna starts counts as online
			sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if s, err := tw.Stream(sctx, tw.UserID()); err == nil && s.Live {
				vt.online(s.StartedAt.Format(time.RFC3339))
			}
		}()
	}
	caps, err = startCapsules(p, st, publish, host, cfg.Get)
	if err != nil {
		return fmt.Errorf("capsules: %w", err)
	}
	router := &chatRouter{rt: caps, host: host, policy: func() *perms.Policy { return policyFrom(cfg.Get()) },
		own: func(id, text string) bool { return th != nil && th.own(id, text) }}
	chatSub := b.Subscribe(256, "twitch.chat.message")
	go router.run(chatSub)

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
		case "plugin":
			return pluginCLI(ctx, caps, host, req.Args)
		case "configcheck":
			return checkPluginSetting(caps, req.Args)
		case "status":
			return statusText(caps, g.Headroom()) + "\n" + oscState(oc), nil
		case "osc":
			return oscCmd(oc, req.Args)
		case "integrations":
			return integrationsCmd(oc, req.Args)
		case "logs":
			return ringSince(daemonLog, req.Args)
		case "recent":
			return ringSince(recentEvents, req.Args)
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
	chatSub.Close()
	hrSub.Close()
	printSub.Close()
	logSub.Close()
	<-logged // flush the event log before the database closes
	return nil
}

// startTwitch connects the broadcaster account and, if logged in, the bot account,
// which then reads and sends chat. Without a broadcaster login Luna runs offline.
// ponytail: client ID and tokens are read once; changing them needs a restart.
func startTwitch(ctx context.Context, p paths, clientID string, g *guard.Guard, publish func(event.Event)) (*twitch.Client, *twitchHost) {
	store := secrets.Open(p.data)
	if !store.UsesKeyring() {
		slog.Warn("OS keyring unreachable from this session; using encrypted token files instead. " +
			"Logins saved to the keyring from a desktop session aren't visible here: log in from this session or start luna from the desktop")
	}
	bc, err := twitch.NewClient(clientID, store, "broadcaster")
	if err != nil {
		slog.Warn("running without twitch", "err", err)
		twitchState = "offline: " + err.Error()
		return nil, nil
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
	login := func(account string) string {
		if t, err := twitch.LoadToken(store, account); err == nil {
			return t.Login
		}
		return "?"
	}
	twitchState = "connected as " + login("broadcaster")
	if bot, err := twitch.NewClient(clientID, store, "bot"); err == nil {
		chatter = bot
		twitchState += ", chatting as " + login("bot")
		run(bot, []twitch.Sub{twitch.ChatSub(b, bot.UserID())})
		run(bc, twitch.ChannelSubs(b))
	} else {
		slog.Info("no bot account; chatting as the broadcaster", "hint", "luna auth login bot")
		run(bc, append(twitch.ChannelSubs(b), twitch.ChatSub(b, b)))
	}
	th := &twitchHost{bc: bc, chatter: chatter}
	g.Live = th.send
	return bc, th
}
