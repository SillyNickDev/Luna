// Package config loads luna.toml. It is validated before use; a bad reload keeps the last good config.
package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/fsnotify/fsnotify"

	"luna/internal/perms"
)

type Config struct {
	Twitch struct {
		ClientID string `toml:"client_id"` // Luna's public Twitch app; override to use your own
	} `toml:"twitch"`
	Chat struct {
		RateLimit int              `toml:"rate_limit"` // messages per 30 s
		Blocklist []string         `toml:"blocklist"`  // RE2 patterns, case-insensitive
		Patterns  []*regexp.Regexp `toml:"-"`
	} `toml:"chat"`
	Perms struct {
		SameRankOverride map[string]bool `toml:"same_rank_override"`
	} `toml:"perms"`
	OSC struct {
		Enabled     bool   `toml:"enabled"`
		Send        string `toml:"send"`       // VRChat's input port
		Listen      string `toml:"listen"`     // VRChat's output port
		ConfigDir   string `toml:"config_dir"` // VRChat's OSC folder, for avatar parameters; found by itself under Proton or Windows
		AllowRemote bool   `toml:"allow_remote"`
	} `toml:"osc"`
	Spotify struct {
		ClientID string `toml:"client_id"` // your app at developer.spotify.com, with redirect URI http://127.0.0.1:43219/callback
	} `toml:"spotify"`
	Index struct {
		URL       string `toml:"url"`        // where index.json and the .capsule files are, https
		PublicKey string `toml:"public_key"` // the index maintainer's Ed25519 key, base64
	} `toml:"index"`
	MemoryLimitMiB int `toml:"memory_limit_mib"`
	// Plugins holds each plugin's settings, keyed by its CLI name, the last part of its ID,
	// or its full ID. They're checked against the plugin's [config] schema when it starts.
	Plugins map[string]map[string]any `toml:"plugins"`
}

func Default() *Config {
	c := &Config{MemoryLimitMiB: 48}
	c.Twitch.ClientID = "9ye8kbj2uo87wxll4l9qsuoowt7yub"
	c.Chat.RateLimit = 20
	c.OSC.Send, c.OSC.Listen = "127.0.0.1:9000", "127.0.0.1:9001"
	return c
}

func (c *Config) Validate() error {
	var errs []error
	if c.Twitch.ClientID == "" {
		errs = append(errs, errors.New("twitch.client_id can't be empty"))
	}
	if c.Chat.RateLimit < 1 || c.Chat.RateLimit > 100 {
		errs = append(errs, fmt.Errorf("chat.rate_limit must be 1-100, got %d", c.Chat.RateLimit))
	}
	c.Chat.Patterns = nil
	for _, p := range c.Chat.Blocklist {
		if len(p) > 200 {
			errs = append(errs, fmt.Errorf("chat.blocklist pattern longer than 200 chars: %.20q…", p))
			continue
		}
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			errs = append(errs, fmt.Errorf("chat.blocklist: %w", err))
			continue
		}
		c.Chat.Patterns = append(c.Chat.Patterns, re)
	}
	for k := range c.Perms.SameRankOverride {
		if _, err := perms.ParseRank(k); err != nil {
			errs = append(errs, fmt.Errorf("perms.same_rank_override: %w", err))
		}
	}
	for _, a := range []struct{ key, addr string }{{"osc.send", c.OSC.Send}, {"osc.listen", c.OSC.Listen}} {
		host, _, err := net.SplitHostPort(a.addr)
		if err != nil && (a.addr != "" || a.key == "osc.send") {
			errs = append(errs, fmt.Errorf("%s: want host:port, got %q", a.key, a.addr))
		} else if ip := net.ParseIP(host); a.addr != "" && !c.OSC.AllowRemote && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			// OSC is plain UDP with no authentication: anyone on the path can read or forge it.
			errs = append(errs, fmt.Errorf("%s: %s isn't this machine; OSC has no encryption, so set osc.allow_remote = true to accept that", a.key, a.addr))
		}
	}
	if c.MemoryLimitMiB < 16 {
		errs = append(errs, fmt.Errorf("memory_limit_mib must be at least 16, got %d", c.MemoryLimitMiB))
	}
	return errors.Join(errs...)
}

// Load reads path; a missing file means defaults.
func Load(path string) (*Config, error) {
	c := Default()
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, c.Validate()
	}
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s is writable by other users; run: chmod go-w %s", path, path)
	}
	md, err := toml.DecodeFile(path, c)
	if err != nil {
		return nil, err
	}
	var unknown []toml.Key
	for _, k := range md.Undecoded() {
		if k[0] != "plugins" { // tables inside plugin settings ([[plugins.avatar.redeem]]) are checked by the plugin's schema
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown keys in %s: %v", path, unknown)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Holder is the live config, swapped atomically on reload.
type Holder struct {
	path string
	cur  atomic.Pointer[Config]
}

func NewHolder(path string) (*Holder, error) {
	h := &Holder{path: path}
	return h, h.Reload()
}

func (h *Holder) Get() *Config { return h.cur.Load() }

func (h *Holder) Reload() error {
	c, err := Load(h.path)
	if err != nil {
		return err
	}
	h.cur.Store(c)
	return nil
}

// Watch reloads when the config file changes, until ctx ends. It watches the
// directory, since editors often save by replacing the file. done gets each reload's result.
func (h *Holder) Watch(ctx context.Context, done func(error)) error {
	dir := filepath.Dir(h.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(dir); err != nil {
		w.Close()
		return err
	}
	go func() {
		defer w.Close()
		var settle <-chan time.Time // editors write in bursts; reload once they stop
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-w.Events:
				if filepath.Clean(ev.Name) == filepath.Clean(h.path) {
					settle = time.After(200 * time.Millisecond)
				}
			case err := <-w.Errors:
				done(err)
			case <-settle:
				done(h.Reload())
			}
		}
	}()
	return nil
}
