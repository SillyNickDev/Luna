package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"luna/internal/capsule"
	"luna/internal/config"
	"luna/internal/plugin"
)

const lunaVersion = "0.1.0-dev"

func versionText() string {
	rev := ""
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				rev = " (" + s.Value[:7] + ")"
			}
		}
	}
	return fmt.Sprintf("luna %s%s\nplugin API %s\ncapsule spec %s", lunaVersion, rev, plugin.API, capsule.SpecVersion)
}

// daemonLog keeps Luna's own recent log lines for luna logs.
var daemonLog = capsule.NewRing(1 << 20)

// recentEvents keeps the last events as JSON lines for luna events tail and chat tail.
var recentEvents = capsule.NewRing(1 << 20)

var started = time.Now()

// twitchState is a one-line summary for luna status, set when Twitch starts.
var twitchState = "offline"

func statusText(caps *capsule.Runtime, headroom float64) string {
	var running, other []string
	for _, c := range caps.List() {
		if st := c.State(); st == capsule.Running {
			running = append(running, c.ID)
		} else {
			other = append(other, c.ID+" ("+st+")")
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "daemon    up %s\n", time.Since(started).Round(time.Second))
	fmt.Fprintf(&b, "twitch    %s\n", twitchState)
	fmt.Fprintf(&b, "chat      %.0f messages available before the rate limit\n", headroom)
	fmt.Fprintf(&b, "capsules  %d running", len(running))
	if len(other) > 0 {
		fmt.Fprintf(&b, "; not running: %s", strings.Join(other, ", "))
	}
	return b.String()
}

// follow polls a daemon ring ("logs" or "events") and prints new lines until Ctrl-C.
// shortcut: the control socket is one request, one reply, so tails poll every 300 ms.
func follow(p paths, cmd string, args []string, print func(string)) error {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	seen := -1 // first poll: only remember where the ring is, unless asked for history
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		out, err := callOut(p, request{Cmd: cmd, Args: append([]string{"--after", strconv.Itoa(max(seen, 0))}, args...)})
		if err != nil {
			return err
		}
		head, rest, _ := strings.Cut(out, "\n")
		n, _ := strconv.Atoi(head)
		if seen >= 0 && rest != "" {
			for l := range strings.Lines(rest) {
				print(strings.TrimSuffix(l, "\n"))
			}
		}
		seen = n
		select {
		case <-stop:
			return nil
		case <-t.C:
		}
	}
}

func ringSince(r *capsule.Ring, args []string) (string, error) {
	n := 0
	if len(args) >= 2 && args[0] == "--after" {
		n, _ = strconv.Atoi(args[1])
	}
	lines, total := r.Since(n)
	return strconv.Itoa(total) + "\n" + strings.Join(lines, "\n"), nil
}

func logsCLI(p paths, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	f := fs.Bool("f", false, "follow")
	capsuleName := fs.String("capsule", "", "a capsule's log instead of Luna's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *capsuleName != "" {
		if *f {
			return errors.New("use luna capsule dev to follow a plugin you're writing; -f with --capsule isn't supported yet")
		}
		return call(p, request{Cmd: "capsule", Args: []string{"logs", *capsuleName}})
	}
	if !*f {
		out, err := callOut(p, request{Cmd: "logs"})
		if err != nil {
			return err
		}
		_, rest, _ := strings.Cut(out, "\n")
		fmt.Println(rest)
		return nil
	}
	return follow(p, "logs", nil, func(l string) { fmt.Println(l) })
}

// eventsTail prints bus events as they happen; chat tail is the same, filtered to chat.
func eventsTail(p paths, args []string, chatOnly bool) error {
	fs := flag.NewFlagSet("tail", flag.ContinueOnError)
	typ := fs.String("type", "", "only these event types, e.g. twitch.*")
	asJSON := fs.Bool("json", false, "JSON lines")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if chatOnly {
		*typ = "twitch.chat.message"
	}
	return follow(p, "recent", nil, func(l string) {
		var ev struct {
			Type      string          `json:"type"`
			Simulated bool            `json:"simulated"`
			TS        time.Time       `json:"ts"`
			Data      json.RawMessage `json:"data"`
		}
		if json.Unmarshal([]byte(l), &ev) != nil || (*typ != "" && !covers(*typ, ev.Type)) {
			return
		}
		switch {
		case *asJSON:
			fmt.Println(l)
		case chatOnly:
			var d struct {
				User struct{ Login, Rank string }
				Text string
			}
			json.Unmarshal(ev.Data, &d)
			sim := ""
			if ev.Simulated {
				sim = " (sim)"
			}
			fmt.Printf("%s %s%s: %s\n", ev.TS.Local().Format("15:04:05"), d.User.Login, sim, d.Text)
		default:
			fmt.Printf("%s %-28s %s\n", ev.TS.Local().Format("15:04:05"), ev.Type, ev.Data)
		}
	})
}

// --- config ---

var keyRe = regexp.MustCompile(`^[a-z0-9_-]+(\.[a-z0-9_-]+)+$`)

func configCLI(p paths, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: luna config show [key] | get <key> | set <key> <value> | edit | validate")
	}
	switch args[0] {
	case "validate":
		if _, err := config.Load(p.config); err != nil {
			return err
		}
		fmt.Println("config ok:", p.config)
		return nil
	case "show", "get":
		c, err := config.Load(p.config)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		if err := toml.NewEncoder(&buf).Encode(c); err != nil {
			return err
		}
		if len(args) == 1 {
			fmt.Print(buf.String())
			return nil
		}
		var all map[string]any
		toml.Decode(buf.String(), &all)
		v, ok := lookup(all, strings.Split(args[1], "."))
		if !ok {
			return exitErr{exitUsage, fmt.Sprintf("no config key %q", args[1])}
		}
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
		return nil
	case "set":
		if len(args) != 3 || !keyRe.MatchString(args[1]) {
			return exitErr{exitUsage, "usage: luna config set <table.key> <value>, e.g. luna config set chat.rate_limit 18"}
		}
		if parts := strings.SplitN(args[1], ".", 3); parts[0] == "plugins" && len(parts) == 3 {
			// Plugin settings are checked against the plugin's own schema, which the daemon knows.
			if _, err := callOut(p, request{Cmd: "configcheck", Args: []string{parts[1], parts[2], args[2]}}); err != nil {
				var ee exitErr
				if errors.As(err, &ee) && ee.code == 3 {
					fmt.Println("note: the daemon isn't running, so this plugin setting isn't checked until the plugin starts")
				} else {
					return err
				}
			}
		}
		return configSet(p.config, args[1], args[2])
	case "edit":
		return configEdit(p.config)
	}
	return exitErr{exitUsage, "unknown config command " + args[0]}
}

func lookup(m map[string]any, path []string) (any, bool) {
	var v any = m
	for _, k := range path {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = mm[k]; !ok {
			return nil, false
		}
	}
	return v, true
}

// tomlValue writes a CLI value as a TOML literal: numbers and booleans as such, the rest as strings.
func tomlValue(s string) string {
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return s
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return s
	}
	if s == "true" || s == "false" {
		return s
	}
	b, _ := json.Marshal(s) // a JSON string is a valid TOML basic string
	return string(b)
}

// configSet changes one key in luna.toml by editing that line, so comments and layout stay.
// The result must validate; otherwise the file is left as it was.
func configSet(path, key, value string) error {
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dot := strings.LastIndexByte(key, '.')
	table, name := key[:dot], key[dot+1:]
	line := name + " = " + tomlValue(value)
	lines := strings.Split(string(old), "\n")
	header := regexp.MustCompile(`^\s*\[\s*` + regexp.QuoteMeta(table) + `\s*\]\s*(#.*)?$`)
	anyHeader := regexp.MustCompile(`^\s*\[`)
	keyLine := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(name) + `\s*=`)
	in, done, insertAt := false, false, -1
	for i, l := range lines {
		switch {
		case header.MatchString(l):
			in, insertAt = true, i+1
		case anyHeader.MatchString(l):
			in = false
		case in && keyLine.MatchString(l):
			lines[i], done = line, true
		case in && strings.TrimSpace(l) != "":
			insertAt = i + 1
		}
		if done {
			break
		}
	}
	switch {
	case done:
	case insertAt >= 0:
		lines = append(lines[:insertAt], append([]string{line}, lines[insertAt:]...)...)
	default:
		lines = append(lines, "", "["+table+"]", line)
	}
	next := strings.Join(lines, "\n")
	if !strings.HasSuffix(next, "\n") {
		next += "\n"
	}
	return writeValidated(path, []byte(next))
}

// writeValidated replaces the config only if the new contents load cleanly.
func writeValidated(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if _, err := config.Load(tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("not saved, the config would be invalid: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	fmt.Println("saved; a running daemon picks it up automatically")
	return nil
}

func configEdit(path string) error {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "luna-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	tmp.Write(old)
	tmp.Close()
	for {
		// $EDITOR may carry flags ("code -w"); split on spaces, never through a shell.
		parts := strings.Fields(editor)
		cmd := exec.Command(parts[0], append(parts[1:], tmp.Name())...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("editor: %w", err)
		}
		b, _ := os.ReadFile(tmp.Name())
		if bytes.Equal(b, old) {
			fmt.Println("no changes")
			return nil
		}
		if _, err := config.Load(tmp.Name()); err == nil {
			return writeValidated(path, b)
		} else {
			fmt.Printf("invalid: %v\nEdit again? [Y/n] ", err)
			var a string
			fmt.Scanln(&a)
			if strings.ToLower(a) == "n" {
				fmt.Println("kept the last good config")
				return nil
			}
		}
	}
}

// --- service ---

func unitPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "systemd", "user", "luna.service"), nil
}

func serviceCLI(verb string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("no systemctl here; run luna daemon yourself (systemd user services are the supported way)")
	}
	if u, _ := unitPath(); u != "" {
		if _, err := os.Stat(u); err != nil {
			return errors.New("the service isn't installed; run: luna setup service")
		}
	}
	cmd := exec.Command("systemctl", "--user", verb, "luna.service")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
