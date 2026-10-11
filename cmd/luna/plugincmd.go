package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"luna/internal/bus"
	"luna/internal/capsule"
	"luna/internal/perms"
	"luna/internal/plugin"
	"luna/internal/plugin/pb"
)

// exitErr carries one of the CLI spec's exit codes back to the shell.
type exitErr struct {
	code int
	msg  string
}

func (e exitErr) Error() string { return e.msg }

const (
	exitUsage      = 2
	exitNotRunning = 4
)

// manifests caches each package's plugin.toml by store hash.
var manifests sync.Map

func pluginManifest(c *capsule.Capsule) *plugin.Manifest {
	if m, ok := manifests.Load(c.Pkg.Hash); ok {
		return m.(*plugin.Manifest)
	}
	m, err := plugin.Load(c.Pkg.Dir)
	if err != nil {
		return &plugin.Manifest{}
	}
	m.ID = c.ID
	manifests.Store(c.Pkg.Hash, m)
	return m
}

// claimsClash reports a chat command or CLI name another running plugin already has.
func claimsClash(rt *capsule.Runtime, id string, m *plugin.Manifest) error {
	for _, c := range rt.List() {
		if c.ID == id {
			continue
		}
		o := pluginManifest(c)
		if m.CLI.Name != "" && m.CLI.Name == o.CLI.Name {
			return fmt.Errorf("%s already uses the command group %q", c.ID, m.CLI.Name)
		}
		for _, a := range m.Chat.Commands {
			for _, b := range o.Chat.Commands {
				for _, n := range append([]string{a.Name}, a.Aliases...) {
					if n == b.Name || slices.Contains(b.Aliases, n) {
						return fmt.Errorf("%s already has the chat command !%s", c.ID, n)
					}
				}
			}
		}
	}
	return nil
}

// pluginCLI serves `luna <name> <command> ...`: Luna checks the arguments against the
// manifest, so bad input never reaches the plugin, then renders what it returns.
func pluginCLI(ctx context.Context, rt *capsule.Runtime, host *plugin.Host, args []string) (string, error) {
	group, args := args[0], args[1:]
	var c *capsule.Capsule
	var m *plugin.Manifest
	for _, cand := range rt.List() {
		pm := pluginManifest(cand)
		if pm.CLI.Name != "" && (pm.CLI.Name == group || cand.ID == group) {
			c, m = cand, pm
		}
	}
	if c == nil {
		return "", exitErr{exitUsage, fmt.Sprintf("unknown command %q (see luna --help; plugin commands need the plugin enabled)", group)}
	}
	asJSON := slices.Contains(args, "--json")
	args = slices.DeleteFunc(args, func(a string) bool { return a == "--json" })
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		return pluginHelp(m), nil
	}
	var cmd *plugin.CLICommand
	for i := range m.CLI.Commands {
		if m.CLI.Commands[i].Name == args[0] {
			cmd = &m.CLI.Commands[i]
		}
	}
	if cmd == nil {
		return "", exitErr{exitUsage, fmt.Sprintf("luna %s has no command %q\n%s", m.CLI.Name, args[0], pluginHelp(m))}
	}
	values, err := parseCLIArgs(cmd, args[1:])
	if err != nil {
		return "", exitErr{exitUsage, fmt.Sprintf("%v\nusage: %s", err, usageLine(m.CLI.Name, cmd))}
	}
	raw, _ := json.Marshal(values)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := host.Invoke(ctx, c.ID, &pb.Command{Kind: "cli", Name: cmd.Name, ArgsJson: raw})
	if errors.Is(err, plugin.ErrNotRunning) {
		msg := c.ID + " isn't running (" + c.State() + ")"
		if h := c.History(); len(h) > 0 {
			msg += "; last failure: " + h[len(h)-1].Cause
		}
		return "", exitErr{exitNotRunning, msg}
	}
	if err != nil {
		return "", err
	}
	if r.Error != "" {
		return "", errors.New(r.Error)
	}
	if len(r.DataJson) == 0 {
		return r.Text, nil
	}
	if asJSON {
		return string(r.DataJson), nil
	}
	return render(r.DataJson), nil
}

func parseCLIArgs(cmd *plugin.CLICommand, args []string) (map[string]any, error) {
	values := map[string]any{}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, ok := strings.CutPrefix(a, "--")
		if !ok {
			pos = append(pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(name, "=")
		var spec *plugin.Arg
		for j := range cmd.Flags {
			if cmd.Flags[j].Name == name {
				spec = &cmd.Flags[j]
			}
		}
		if spec == nil {
			return nil, fmt.Errorf("unknown flag --%s", name)
		}
		if spec.Type == "bool" && !hasVal {
			values[name] = true
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--%s needs a value", name)
			}
			i++
			val = args[i]
		}
		v, err := parseValue(spec.Type, val)
		if err != nil {
			return nil, fmt.Errorf("--%s: %v", name, err)
		}
		values[name] = v
	}
	if len(pos) > len(cmd.Args) {
		return nil, errors.New("too many arguments")
	}
	for i, spec := range cmd.Args {
		if i >= len(pos) {
			if !spec.Optional {
				return nil, fmt.Errorf("missing <%s>", spec.Name)
			}
			continue
		}
		v, err := parseValue(spec.Type, pos[i])
		if err != nil {
			return nil, fmt.Errorf("<%s>: %v", spec.Name, err)
		}
		values[spec.Name] = v
	}
	return values, nil
}

func parseValue(typ, s string) (any, error) {
	switch typ {
	case "int":
		return strconv.ParseInt(s, 10, 64)
	case "float":
		return strconv.ParseFloat(s, 64)
	case "bool":
		return strconv.ParseBool(s)
	}
	return s, nil
}

func usageLine(group string, c *plugin.CLICommand) string {
	parts := []string{"luna", group, c.Name}
	for _, a := range c.Args {
		if a.Optional {
			parts = append(parts, "["+a.Name+"]")
		} else {
			parts = append(parts, "<"+a.Name+">")
		}
	}
	for _, f := range c.Flags {
		if f.Type == "bool" {
			parts = append(parts, "[--"+f.Name+"]")
		} else {
			parts = append(parts, "[--"+f.Name+" "+f.Type+"]")
		}
	}
	return strings.Join(parts, " ")
}

func pluginHelp(m *plugin.Manifest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "commands from %s:\n", m.ID)
	for _, c := range m.CLI.Commands {
		fmt.Fprintf(&b, "  %-40s %s\n", usageLine(m.CLI.Name, &c), c.Summary)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// render shows a command's data as a table: an object as key/value rows, a list of
// objects as columns, anything else as JSON.
func render(raw []byte) string {
	var v any
	json.Unmarshal(raw, &v)
	cell := func(x any) string {
		if s, ok := x.(string); ok {
			return s
		}
		b, _ := json.Marshal(x)
		return string(b)
	}
	var b strings.Builder
	switch v := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		w := 0
		for k := range v {
			keys = append(keys, k)
			w = max(w, len(k))
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%-*s  %s\n", w, k, cell(v[k]))
		}
	case []any:
		var cols []string
		for _, row := range v {
			if m, ok := row.(map[string]any); ok {
				for k := range m {
					if !slices.Contains(cols, k) {
						cols = append(cols, k)
					}
				}
			} else {
				fmt.Fprintln(&b, cell(row))
			}
		}
		sort.Strings(cols)
		if len(cols) > 0 {
			rows := [][]string{cols}
			for _, row := range v {
				if m, ok := row.(map[string]any); ok {
					r := make([]string, len(cols))
					for i, c := range cols {
						if x, ok := m[c]; ok {
							r[i] = cell(x)
						}
					}
					rows = append(rows, r)
				}
			}
			w := make([]int, len(cols))
			for _, r := range rows {
				for i, c := range r {
					w[i] = max(w[i], len(c))
				}
			}
			for _, r := range rows {
				for i, c := range r {
					fmt.Fprintf(&b, "%-*s  ", w[i], c)
				}
				b.WriteString("\n")
			}
		}
	default:
		return cell(v)
	}
	return strings.TrimRight(b.String(), "\n ")
}

// chatRouter turns "!command" chat messages into plugin calls. Luna checks the sender's
// capability and the cooldowns first, so a plugin only sees calls it should act on.
type chatRouter struct {
	rt     *capsule.Runtime
	host   *plugin.Host
	policy func() *perms.Policy
	own    func(userID, text string) bool // a message Luna itself sent: never a command

	mu   sync.Mutex
	last map[string]time.Time // cooldown key → last use
}

// invisible strips characters that make two commands look alike but compare different.
// shortcut: no NFKC folding yet (needs golang.org/x/text); add with the Moderation filters.
func invisible(r rune) rune {
	switch {
	case r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E, r >= 0x2060 && r <= 0x2064, r == 0xFEFF:
		return -1
	}
	return r
}

func (cr *chatRouter) run(sub *bus.Sub) {
	for ev := range sub.C {
		var d struct {
			User struct {
				ID   string `json:"id"`
				Rank string `json:"rank"`
			} `json:"user"`
			Text string `json:"text"`
		}
		if json.Unmarshal(ev.Data, &d) != nil || !strings.HasPrefix(d.Text, "!") || cr.own(d.User.ID, d.Text) {
			continue
		}
		words := strings.Fields(strings.Map(invisible, d.Text))
		if len(words) == 0 || len(words) > 21 {
			continue
		}
		name := strings.ToLower(strings.TrimPrefix(words[0], "!"))
		c, cmd := cr.find(name)
		if c == nil {
			continue
		}
		rank, err := perms.ParseRank(d.User.Rank)
		if err != nil {
			rank = perms.Viewer
		}
		capability := cmd.Capability
		if capability == "" {
			capability = "chat.commands.use"
		}
		if !cr.policy().Can(d.User.ID, rank, capability) || !cr.cooldown(c.ID, cmd, d.User.ID) {
			continue
		}
		args, _ := json.Marshal(map[string]any{"args": words[1:]})
		pe := &pb.Event{Id: ev.ID, Type: ev.Type, Source: ev.Source, TsMs: ev.TS.UnixMilli(), Simulated: ev.Simulated, DataJson: ev.Data}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r, err := cr.host.Invoke(ctx, c.ID, &pb.Command{Kind: "chat", Name: cmd.Name, ArgsJson: args, Event: pe})
			if err == nil && r.Error != "" {
				err = errors.New(r.Error)
			}
			if err != nil {
				slog.Warn("chat command failed", "plugin", c.ID, "command", cmd.Name, "err", err)
			}
		}()
	}
}

func (cr *chatRouter) find(name string) (*capsule.Capsule, *plugin.ChatCommand) {
	for _, c := range cr.rt.List() {
		m := pluginManifest(c)
		for i, cmd := range m.Chat.Commands {
			if cmd.Name == name || slices.Contains(cmd.Aliases, name) {
				return c, &m.Chat.Commands[i]
			}
		}
	}
	return nil, nil
}

// cooldown records a use and reports whether the command was off cooldown, globally and for this user.
func (cr *chatRouter) cooldown(id string, cmd *plugin.ChatCommand, user string) bool {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	now := time.Now()
	g, u := id+"\x00"+cmd.Name, id+"\x00"+cmd.Name+"\x00"+user
	if now.Sub(cr.last[g]) < time.Duration(cmd.Cooldown.Global) || now.Sub(cr.last[u]) < time.Duration(cmd.Cooldown.User) {
		return false
	}
	if cr.last == nil || len(cr.last) > 10000 {
		cr.last = map[string]time.Time{} // bounded: forgetting old cooldowns early is harmless
	}
	cr.last[g], cr.last[u] = now, now
	return true
}

// restartChangedPlugins restarts plugins whose [plugins.<name>] settings changed, since a
// plugin reads its config when it starts.
func restartChangedPlugins(rt *capsule.Runtime, before, after map[string]map[string]any) {
	if rt == nil {
		return
	}
	for _, c := range rt.List() {
		m := pluginManifest(c)
		if m.ID == "" {
			continue
		}
		a, _ := json.Marshal(m.Overrides(before))
		b, _ := json.Marshal(m.Overrides(after))
		if string(a) != string(b) {
			slog.Info("plugin settings changed; restarting it", "plugin", c.ID)
			c.Start()
		}
	}
}

// checkPluginSetting validates `luna config set plugins.<name>.<key> <value>` against the
// installed plugin's [config] before the file changes.
func checkPluginSetting(rt *capsule.Runtime, args []string) (string, error) {
	if len(args) != 3 {
		return "", errors.New("usage: configcheck <plugin> <key> <value>")
	}
	for _, c := range rt.List() {
		m := pluginManifest(c)
		if m.ID == "" || m.Overrides(map[string]map[string]any{args[0]: {}}) == nil {
			continue
		}
		f, ok := m.Config[args[1]]
		if !ok {
			var keys []string
			for k := range m.Config {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if len(keys) == 0 {
				return "", fmt.Errorf("%s has no settings", c.ID)
			}
			return "", fmt.Errorf("%s has no setting %q (it has: %s)", c.ID, args[1], strings.Join(keys, ", "))
		}
		var v any
		if err := json.Unmarshal([]byte(args[2]), &v); err != nil {
			v = args[2]
		}
		if n, ok := v.(float64); ok && f.Type == "int" {
			v = n
		}
		if _, err := plugin.Coerce(f.Type, v); err != nil {
			return "", fmt.Errorf("%s.%s: %v", args[0], args[1], err)
		}
		return "ok", nil
	}
	return "", fmt.Errorf("no running plugin is called %q", args[0])
}
