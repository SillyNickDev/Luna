// Package config loads luna.toml. It is validated before use; a bad reload keeps the last good config.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"runtime"
	"sync/atomic"

	"github.com/BurntSushi/toml"

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
	MemoryLimitMiB int `toml:"memory_limit_mib"`
}

func Default() *Config {
	c := &Config{MemoryLimitMiB: 48}
	c.Twitch.ClientID = "9ye8kbj2uo87wxll4l9qsuoowt7yub"
	c.Chat.RateLimit = 20
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
	if und := md.Undecoded(); len(und) > 0 {
		return nil, fmt.Errorf("unknown keys in %s: %v", path, und)
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
