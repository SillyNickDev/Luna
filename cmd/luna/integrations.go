package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"luna/internal/bus"
	"luna/internal/config"
	"luna/internal/deepl"
	"luna/internal/event"
	"luna/internal/osc"
	"luna/internal/plugin"
	"luna/internal/secrets"
	"luna/internal/spotify"
)

// startOSC connects to VRChat when [osc] is enabled.
// shortcut: [osc] is read at startup; changing it needs a restart.
func startOSC(ctx context.Context, cfg *config.Config, publish func(event.Event)) *osc.Client {
	if !cfg.OSC.Enabled {
		return nil
	}
	dir := cfg.OSC.ConfigDir
	if dir == "" {
		dir = osc.DefaultConfigDir()
	}
	oc, err := osc.Dial(cfg.OSC.Send, cfg.OSC.Listen, dir, publish)
	if err != nil {
		slog.Error("osc off", "err", err)
		return nil
	}
	if dir == "" {
		slog.Warn("osc: VRChat's OSC folder not found, so avatar parameters can't be listed; set osc.config_dir")
	}
	go oc.Run(ctx)
	return oc
}

func oscState(oc *osc.Client) string {
	switch {
	case oc == nil:
		return "osc       off ([osc] enabled = true in luna.toml)"
	case !oc.Listening():
		return "osc       sending only: the listen port is taken, so Luna doesn't see avatar changes"
	case oc.LastHeard().IsZero():
		return "osc       waiting for VRChat (is OSC on in VRChat's action menu?)"
	}
	a := oc.Avatar()
	s := fmt.Sprintf("osc       VRChat heard %s ago", time.Since(oc.LastHeard()).Round(time.Second))
	if a.ID != "" {
		s += fmt.Sprintf("; avatar %s, %d parameters", or(a.Name, a.ID), len(a.Params))
	}
	return s
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// oscCmd is `luna osc send <address> <value>` and `luna osc params`.
func oscCmd(oc *osc.Client, args []string) (string, error) {
	if oc == nil {
		return "", errors.New("OSC is off; turn it on with [osc] enabled = true in luna.toml and restart luna")
	}
	switch {
	case len(args) == 3 && args[0] == "send":
		var v any = args[2]
		if b, err := strconv.ParseBool(args[2]); err == nil {
			v = b
		} else if n, err := strconv.Atoi(args[2]); err == nil {
			v = n
		} else if f, err := strconv.ParseFloat(args[2], 32); err == nil {
			v = float32(f)
		}
		if err := oc.Send(args[1], v); err != nil {
			return "", err
		}
		return fmt.Sprintf("sent %s %v", args[1], v), nil
	case len(args) == 1 && args[0] == "params":
		a := oc.Avatar()
		if a.ID == "" {
			return "", errors.New("no avatar yet: VRChat says which avatar you're wearing when you change into one")
		}
		b, _ := json.MarshalIndent(a, "", "  ")
		return string(b), nil
	}
	return "", errors.New("usage: luna osc send <address> <value> | luna osc params")
}

func integrationsCmd(oc *osc.Client, args []string) (string, error) {
	switch {
	case len(args) == 0 || args[0] == "list":
		return oscState(oc) + "\nhr        from hr.update events (luna sim hr <bpm>); the HRCollect client isn't built yet", nil
	case len(args) == 2 && args[0] == "test" && args[1] == "osc":
		if oc == nil {
			return "", errors.New("OSC is off; turn it on with [osc] enabled = true in luna.toml and restart luna")
		}
		oc.Chatbox("Luna is connected ✓")
		return "sent a test message to the VRChat chatbox; check it shows in game. " + oscState(oc), nil
	}
	return "", errors.New("usage: luna integrations list | luna integrations test osc")
}

// feedHR keeps the plugins' heart rate history from hr.update events, simulated ones included.
func feedHR(sub *bus.Sub, hr *plugin.HR) {
	for ev := range sub.C {
		var d struct {
			BPM int `json:"bpm"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.BPM > 0 {
			hr.Add(ev.TS, d.BPM)
		}
	}
}

// spotifyHost answers plugins' Spotify calls with the daemon's Spotify login.
type spotifyHost struct {
	c     *spotify.Client
	mu    sync.Mutex
	lists map[string]playlistCache
}

type playlistCache struct {
	ids map[string]bool
	at  time.Time
}

// startSpotify connects Spotify when it's configured and logged in.
func startSpotify(cfg *config.Config, store *secrets.Store) *spotifyHost {
	if cfg.Spotify.ClientID == "" {
		return nil
	}
	c, err := spotify.NewClient(cfg.Spotify.ClientID, store)
	if err != nil {
		slog.Info("spotify not connected", "err", err)
		return nil
	}
	return &spotifyHost{c: c, lists: map[string]playlistCache{}}
}

func (h *spotifyHost) Call(ctx context.Context, call string, raw []byte) (any, error) {
	var a struct {
		Query, URI, Playlist, Track string
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
	}
	switch call {
	case "find":
		return h.c.Find(ctx, a.Query)
	case "playing":
		return h.c.Playing(ctx)
	case "queue":
		if !strings.HasPrefix(a.URI, "spotify:track:") {
			return nil, errors.New("queue takes a spotify:track: URI")
		}
		return nil, h.c.Queue(ctx, a.URI)
	case "skip":
		return nil, h.c.Skip(ctx)
	case "in_playlist":
		id := spotify.PlaylistID(a.Playlist)
		h.mu.Lock()
		pc, ok := h.lists[id]
		h.mu.Unlock()
		if !ok || time.Since(pc.at) > 10*time.Minute { // the streamer may edit the playlist mid-stream
			ids, err := h.c.PlaylistTracks(ctx, id)
			if err != nil {
				return nil, err
			}
			pc = playlistCache{ids, time.Now()}
			h.mu.Lock()
			h.lists[id] = pc
			h.mu.Unlock()
		}
		return map[string]bool{"in": pc.ids[a.Track]}, nil
	}
	return nil, fmt.Errorf("unknown spotify call %q", call)
}

// deeplHost translates with the DeepL key from the secrets store.
type deeplHost struct{ key string }

func startDeepL(store *secrets.Store) *deeplHost {
	k, err := store.Get("deepl")
	if err != nil {
		return nil
	}
	return &deeplHost{strings.TrimSpace(string(k))}
}

func (d *deeplHost) Translate(ctx context.Context, text, target string) (string, string, error) {
	return deepl.Translate(ctx, d.key, text, target)
}
