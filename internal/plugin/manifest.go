// Package plugin serves the plugin API (api/plugin.proto) on each capsule's host socket
// and reads the host half of plugin.toml: permissions, commands and config.
package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"luna/internal/perms"
)

const API = "1"

type Manifest struct {
	ID          string `toml:"id"`
	Name        string `toml:"name"`
	Version     string `toml:"version"`
	API         string `toml:"api"`
	Permissions struct {
		Events    []string `toml:"events"`
		Chat      []string `toml:"chat"`
		Store     bool     `toml:"store"`
		Mod       []string `toml:"mod"`
		Twitch    []string `toml:"twitch"`
		Viewers   bool     `toml:"viewers"`
		OSC       []string `toml:"osc"`       // "send:/avatar/parameters/Ears" (a trailing * matches a prefix), "chatbox", "read"
		Spotify   []string `toml:"spotify"`   // "read" (search, now playing, playlists), "control" (queue, skip)
		Translate bool     `toml:"translate"` // translations with the streamer's DeepL key
	} `toml:"permissions"`
	CLI struct {
		Name     string       `toml:"name"`
		Commands []CLICommand `toml:"commands"`
	} `toml:"cli"`
	Chat struct {
		Commands []ChatCommand `toml:"commands"`
	} `toml:"chat"`
	Config map[string]Field `toml:"config"`
}

// Arg is a CLI command argument or flag.
type Arg struct {
	Name     string `toml:"name" json:"name"`
	Type     string `toml:"type" json:"type"` // int, float, str, bool
	Optional bool   `toml:"optional" json:"optional,omitempty"`
}

type CLICommand struct {
	Name    string `toml:"name" json:"name"`
	Summary string `toml:"summary" json:"summary,omitempty"`
	Args    []Arg  `toml:"args" json:"args,omitempty"`
	Flags   []Arg  `toml:"flags" json:"flags,omitempty"`
	Mutates bool   `toml:"mutates" json:"mutates,omitempty"`
}

type ChatCommand struct {
	Name       string   `toml:"name"`
	Aliases    []string `toml:"aliases"`
	Capability string   `toml:"capability"`
	Cooldown   struct {
		Global duration `toml:"global"`
		User   duration `toml:"user"`
	} `toml:"cooldown"`
}

type duration time.Duration

func (d *duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	*d = duration(v)
	return err
}

// Field is one [config] entry.
type Field struct {
	Type    string `toml:"type"` // int, float, str, bool, or list (an array of tables, like [[plugins.avatar.redeem]])
	Default any    `toml:"default"`
}

var (
	nameRe    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	patternRe = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)*(\.\*)?$`)
)

// reserved CLI group names; a plugin can't take them.
var reserved = []string{"run", "daemon", "start", "stop", "restart", "status", "reload", "update", "version", "setup",
	"auth", "config", "integrations", "backup", "restore", "export", "events", "sim", "chat", "logs", "mod",
	"osc", "obs", "capsule", "perms", "plugin", "completion", "help", "viewers", "tui", "index"}

// Load reads the host half of dir/plugin.toml. A package with only capsule.toml has no
// permissions and no commands; it can still run and talk over links.
func Load(dir string) (*Manifest, error) {
	m := &Manifest{API: API}
	b, err := os.ReadFile(filepath.Join(dir, "plugin.toml"))
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	} else if err != nil {
		return nil, err
	}
	md, err := toml.Decode(string(b), m)
	if err != nil {
		return nil, fmt.Errorf("plugin.toml: %w", err)
	}
	var errs []error
	for _, k := range md.Undecoded() {
		switch {
		case k[0] == "capsule", k[0] == "links":
			// the runtime's half
		case k[0] == "permissions" && len(k) == 2:
			errs = append(errs, fmt.Errorf("plugin.toml asks for [permissions].%s, which this version of Luna can't grant yet", k[1]))
		case k[0] == "permissions":
		default:
			errs = append(errs, fmt.Errorf("plugin.toml: unknown key %s", k))
		}
	}
	return m, errors.Join(append(errs, m.validate())...)
}

func (m *Manifest) validate() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if m.API != API {
		bad("plugin.toml: api %q isn't supported (this Luna speaks %q)", m.API, API)
	}
	for _, p := range m.Permissions.Events {
		if !patternRe.MatchString(p) {
			bad("[permissions].events: %q must be an event type like twitch.follow or a family like twitch.*", p)
		}
	}
	for _, group := range []struct {
		name      string
		have, all []string
	}{
		{"chat", m.Permissions.Chat, chatPerms},
		{"mod", m.Permissions.Mod, modPerms},
		{"twitch", m.Permissions.Twitch, twitchPerms},
		{"spotify", m.Permissions.Spotify, []string{"read", "control"}},
	} {
		for _, c := range group.have {
			if !slices.Contains(group.all, c) {
				bad("[permissions].%s: %q isn't supported (one of %s)", group.name, c, strings.Join(group.all, ", "))
			}
		}
	}
	for _, o := range m.Permissions.OSC {
		if addr, ok := strings.CutPrefix(o, "send:"); ok {
			if !oscAddrRe.MatchString(strings.TrimSuffix(addr, "*")) {
				bad("[permissions].osc: %q must be send:/an/osc/address, optionally ending in *", o)
			}
		} else if o != "chatbox" && o != "read" {
			bad(`[permissions].osc: %q isn't supported (send:<address>, "chatbox" or "read")`, o)
		}
	}
	if m.CLI.Name != "" && (!nameRe.MatchString(m.CLI.Name) || slices.Contains(reserved, m.CLI.Name)) {
		bad("[cli].name %q must be a short lowercase name and not a built-in command", m.CLI.Name)
	}
	if len(m.CLI.Commands) > 0 && m.CLI.Name == "" {
		bad("[[cli.commands]] needs [cli].name, the command group (luna <name> <command>)")
	}
	seen := map[string]bool{}
	for _, c := range m.CLI.Commands {
		if !nameRe.MatchString(c.Name) || seen[c.Name] {
			bad("[[cli.commands]] name %q must be unique, lowercase, ≤32 chars", c.Name)
		}
		seen[c.Name] = true
		for _, a := range append(slices.Clone(c.Args), c.Flags...) {
			if !nameRe.MatchString(a.Name) || !validType(a.Type) {
				bad("command %s: arg %q needs a lowercase name and a type of int, float, str or bool", c.Name, a.Name)
			}
		}
	}
	seen = map[string]bool{}
	for _, c := range m.Chat.Commands {
		for _, n := range append([]string{c.Name}, c.Aliases...) {
			if !nameRe.MatchString(n) || seen[n] {
				bad("[[chat.commands]] name or alias %q must be unique, lowercase, ≤32 chars", n)
			}
			seen[n] = true
		}
		if c.Capability != "" && !perms.Known(c.Capability) {
			bad("chat command %s: unknown capability %q", c.Name, c.Capability)
		}
	}
	for k, f := range m.Config {
		if !nameRe.MatchString(k) || !validType(f.Type) {
			bad("[config].%s needs a lowercase name and a type of int, float, str, bool or list", k)
		} else if _, err := Coerce(f.Type, f.Default); err != nil {
			bad("[config].%s default: %v", k, err)
		}
	}
	return errors.Join(errs...)
}

func validType(t string) bool {
	return t == "int" || t == "float" || t == "str" || t == "bool" || t == "list"
}

// Coerce checks v against a declared type, accepting the forms TOML and JSON give.
func Coerce(typ string, v any) (any, error) {
	switch typ {
	case "int":
		switch n := v.(type) {
		case int64:
			return n, nil
		case float64:
			if n == math.Trunc(n) && math.Abs(n) < 1<<53 {
				return int64(n), nil
			}
		}
	case "float":
		switch n := v.(type) {
		case int64:
			return float64(n), nil
		case float64:
			return n, nil
		}
	case "str":
		if s, ok := v.(string); ok {
			return s, nil
		}
	case "bool":
		if b, ok := v.(bool); ok {
			return b, nil
		}
	case "list": // tables only, so each entry is a record the plugin decodes into its own struct
		var out []any
		switch l := v.(type) {
		case []any:
			out = l
		case []map[string]any:
			for _, t := range l {
				out = append(out, t)
			}
		default:
			return nil, fmt.Errorf("want a list of tables, got %v (%T)", v, v)
		}
		for i, t := range out {
			if _, ok := t.(map[string]any); !ok {
				return nil, fmt.Errorf("entry %d: want a table, got %v (%T)", i+1, t, t)
			}
		}
		if out == nil {
			out = []any{}
		}
		return out, nil
	}
	return nil, fmt.Errorf("want %s, got %v (%T)", typ, v, v)
}

// EffectiveConfig is the plugin's defaults with the user's overrides from luna.toml applied.
func (m *Manifest) EffectiveConfig(overrides map[string]any) ([]byte, error) {
	out := map[string]any{}
	for k, f := range m.Config {
		out[k], _ = Coerce(f.Type, f.Default)
	}
	var errs []error
	for k, v := range overrides {
		f, ok := m.Config[k]
		if !ok {
			errs = append(errs, fmt.Errorf("plugins.%s.%s: the plugin has no such setting", m.ID, k))
			continue
		}
		c, err := Coerce(f.Type, v)
		if err != nil {
			errs = append(errs, fmt.Errorf("plugins.%s.%s: %v", m.ID, k, err))
			continue
		}
		out[k] = c
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

var (
	chatPerms = []string{"send", "announce", "whisper"}
	modPerms  = []string{"timeout", "ban", "delete", "warn", "shield", "shoutout"}
	// twitchPerms: reading public info, then each kind of change to the channel.
	twitchPerms = []string{"read", "edit", "clips", "markers", "ads", "raid", "polls", "predictions"}
	twitchWords = map[string]string{"read": "reads public Twitch info: users, your channel and stream, who follows",
		"edit": "changes your stream title and category", "clips": "makes clips of your stream",
		"markers": "adds VOD markers", "ads": "reads your ad schedule and snoozes ads", "raid": "starts and cancels raids",
		"polls": "runs Twitch polls", "predictions": "runs Twitch predictions (viewers bet channel points)"}
	// modWords say what each [permissions].mod entry lets the plugin do, for approval.
	modWords = map[string]string{"timeout": "time viewers out", "ban": "ban and unban viewers",
		"delete": "delete chat messages", "warn": "send Twitch warnings", "shield": "turn Shield Mode on and off",
		"shoutout": "send Twitch shoutouts"}
)

// oscAddrRe is an OSC address with no pattern characters, so permissions and sends are literal.
var oscAddrRe = regexp.MustCompile(`^(/[A-Za-z0-9_.\-]+)+/?$`)

// AllowsOSCSend reports whether [permissions].osc lets the plugin send to address.
func (m *Manifest) AllowsOSCSend(address string) bool {
	if !oscAddrRe.MatchString(address) || strings.HasSuffix(address, "/") ||
		slices.ContainsFunc(strings.Split(address, "/"), func(seg string) bool { return seg == "." || seg == ".." }) {
		return false // no dot segments, so a prefix permission can't be stepped out of
	}
	for _, o := range m.Permissions.OSC {
		p, ok := strings.CutPrefix(o, "send:")
		if ok && (p == address || strings.HasSuffix(p, "*") && strings.HasPrefix(address, strings.TrimSuffix(p, "*"))) {
			return true
		}
	}
	return false
}

// AllowsEvents reports whether pattern is within [permissions].events.
func (m *Manifest) AllowsEvents(pattern string) bool {
	for _, p := range m.Permissions.Events {
		if p == pattern || strings.HasSuffix(p, ".*") && strings.HasPrefix(pattern, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

// Describe lists the host permissions in plain words, for the install approval.
func (m *Manifest) Describe() string {
	var b strings.Builder
	if len(m.Permissions.Events) > 0 {
		fmt.Fprintf(&b, "  sees events: %s\n", strings.Join(m.Permissions.Events, ", "))
	}
	if slices.Contains(m.Permissions.Chat, "send") {
		b.WriteString("  sends chat messages\n")
	}
	if slices.Contains(m.Permissions.Chat, "announce") {
		b.WriteString("  posts chat announcements\n")
	}
	if slices.Contains(m.Permissions.Chat, "whisper") {
		b.WriteString("  whispers viewers\n")
	}
	for _, p := range m.Permissions.Mod {
		fmt.Fprintf(&b, "  moderates: can %s\n", modWords[p])
	}
	for _, p := range m.Permissions.Twitch {
		fmt.Fprintf(&b, "  %s\n", twitchWords[p])
	}
	for _, o := range m.Permissions.OSC {
		switch {
		case o == "chatbox":
			b.WriteString("  writes in your VRChat chatbox\n")
		case o == "read":
			b.WriteString("  sees your VRChat avatar and its parameters\n")
		default:
			fmt.Fprintf(&b, "  sets VRChat avatar parameters: %s\n", strings.TrimPrefix(o, "send:"))
		}
	}
	if slices.Contains(m.Permissions.Spotify, "read") {
		b.WriteString("  searches Spotify and sees what's playing on your Spotify\n")
	}
	if slices.Contains(m.Permissions.Spotify, "control") {
		b.WriteString("  adds songs to your Spotify queue and skips songs\n")
	}
	if m.Permissions.Translate {
		b.WriteString("  translates text with your DeepL key (it uses your monthly quota)\n")
	}
	if m.Permissions.Viewers {
		b.WriteString("  reads viewer profiles: first and last seen, messages, watch time, streaks\n")
	}
	if m.Permissions.Store {
		b.WriteString("  keeps its own saved data in Luna's database\n")
	}
	for _, c := range m.Chat.Commands {
		fmt.Fprintf(&b, "  adds the chat command !%s\n", c.Name)
	}
	if m.CLI.Name != "" {
		fmt.Fprintf(&b, "  adds the command group: luna %s\n", m.CLI.Name)
	}
	return b.String()
}

// Overrides finds this plugin's section in luna.toml's [plugins], by CLI name, short name or ID.
func (m *Manifest) Overrides(all map[string]map[string]any) map[string]any {
	short := m.ID[strings.LastIndexByte(m.ID, '.')+1:]
	for _, k := range []string{m.ID, m.CLI.Name, short} {
		if o, ok := all[k]; ok && k != "" {
			return o
		}
	}
	return nil
}
