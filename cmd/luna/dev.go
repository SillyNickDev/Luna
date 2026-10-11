package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/fsnotify/fsnotify"

	"luna/internal/capsule"
	"luna/internal/plugin"
)

var initNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

const pyTemplate = `from luna import bot


@bot.on("twitch.channel.follow")
def welcome(ev):
    bot.chat.send(f"Welcome to the pack, {ev.user.name}!")


bot.run()
`

const pyTestTemplate = `# Run with: python3 -m unittest (or pytest), with Luna's SDK on PYTHONPATH.
import unittest

from luna.testing import harness

import main


class Test(unittest.TestCase):
    def test_welcome(self):
        t = harness(main)
        t.emit("twitch.channel.follow", user="fox")
        self.assertEqual(t.chat.sent, ["Welcome to the pack, fox!"])


if __name__ == "__main__":
    unittest.main()
`

const tsTemplate = `import { bot } from "@luna/sdk";

bot.on("twitch.channel.follow", (ev) => bot.chat.send(` + "`Welcome to the pack, ${ev.user!.name}!`" + `));

bot.run();
`

const tsTestTemplate = `// Run with: node --import <Luna's SDK>/register.mjs --test
import assert from "node:assert/strict";
import { test } from "node:test";
import { harness } from "@luna/sdk/testing";
import "./main.ts";

test("welcome", async () => {
  const t = await harness();
  await t.emit("twitch.channel.follow", { user: "fox" });
  assert.deepEqual(t.chat.sent, ["Welcome to the pack, fox!"]);
});
`

// templates are each runtime's starter files: entry, test.
var templates = map[string][3]string{
	"python-3.12": {"main.py", pyTemplate, "test_main.py\x00" + pyTestTemplate},
	"node-22":     {"main.ts", tsTemplate, "main.test.ts\x00" + tsTestTemplate},
}

// capsuleInit scaffolds a plugin folder: luna capsule init <name> [--runtime python-3.12|node-22].
func capsuleInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	rt := fs.String("runtime", "python-3.12", "runtime: python-3.12 or node-22")
	if err := fs.Parse(args); err != nil {
		return err
	}
	name := fs.Arg(0)
	if fs.NArg() > 1 { // flags may also follow the name
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
	}
	if !initNameRe.MatchString(name) {
		return errors.New("usage: luna capsule init <name> [--runtime python-3.12|node-22]; name is lowercase letters, digits and dashes")
	}
	tpl, ok := templates[*rt]
	if !ok {
		return fmt.Errorf("runtime %s has no template; python-3.12 and node-22 do (Go plugins build as native: see sdk/go)", *rt)
	}
	if _, err := os.Stat(name); err == nil {
		return fmt.Errorf("%s already exists", name)
	}
	manifest := fmt.Sprintf(`id = "dev.local.%s"
version = "0.1.0"
api = "1"

[capsule]
runtime = "%s"
entry = "%s"
limits = { memory = "64m", cpu = 0.25 }

[permissions]
events = ["twitch.channel.follow"]
chat = ["send"]
`, strings.ReplaceAll(name, "-", ""), *rt, tpl[0])
	if err := os.Mkdir(name, 0o755); err != nil {
		return err
	}
	testName, testBody, _ := strings.Cut(tpl[2], "\x00")
	for f, body := range map[string]string{tpl[0]: tpl[1], testName: testBody, "plugin.toml": manifest} {
		if err := os.WriteFile(filepath.Join(name, f), []byte(body), 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("created %s/ (change the id in plugin.toml before you publish)\nnext: cd %s && luna capsule dev\n", name, name)
	return nil
}

// description is what the SDK prints under LUNA_DESCRIBE=1.
type description struct {
	Events []string            `json:"events"`
	CLI    []plugin.CLICommand `json:"cli"`
	Chat   []describedChat     `json:"chat"`
	Config map[string]any      `json:"config"`
	Links  capsule.Links       `json:"links"`
}

type describedChat struct {
	Name         string   `json:"name"`
	Aliases      []string `json:"aliases"`
	Capability   *string  `json:"capability"`
	Cooldown     *string  `json:"cooldown"`
	UserCooldown *string  `json:"user_cooldown"`
}

// generateManifest asks the plugin what it declares and rewrites plugin.toml. The parts the
// author owns (id, version, [capsule], chat and store permissions) are kept; events, commands,
// config and links come from the code. Event permissions the author widened (twitch.*) stay.
func generateManifest(p paths, dir string) error {
	sdks, err := sdkDirs(p)
	if err != nil {
		return err
	}
	out, err := capsule.Describe(dir, p.runDir(), p.runtimes(), sdks)
	if err != nil {
		return err
	}
	var d description
	if err := json.Unmarshal(out, &d); err != nil {
		return fmt.Errorf("the plugin's description isn't JSON: %w", err)
	}
	path := filepath.Join(dir, "plugin.toml")
	var cur map[string]any
	if _, err := toml.DecodeFile(path, &cur); err != nil {
		return err
	}
	perms, _ := cur["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	var kept []string
	for _, e := range anyStrings(perms["events"]) {
		// keep a hand-written entry only while it still covers something the code handles
		if slices.ContainsFunc(d.Events, func(t string) bool { return covers(e, t) }) {
			kept = append(kept, e)
		}
	}
	for _, t := range d.Events {
		if !slices.ContainsFunc(kept, func(e string) bool { return covers(e, t) }) {
			kept = append(kept, t)
		}
	}
	perms["events"] = kept

	var b bytes.Buffer
	b.WriteString("# Regenerated by luna capsule dev. Edit id, version, [capsule] and [permissions] here;\n" +
		"# events, commands, config and links come from the code.\n")
	enc := toml.NewEncoder(&b)
	enc.Indent = ""
	for _, k := range []string{"id", "name", "version", "api"} {
		if v, ok := cur[k]; ok {
			if err := enc.Encode(map[string]any{k: v}); err != nil {
				return err
			}
		}
	}
	section := func(name string, v any) error { return enc.Encode(map[string]any{name: v}) }
	if err := section("capsule", cur["capsule"]); err != nil {
		return err
	}
	if err := section("permissions", perms); err != nil {
		return err
	}
	if len(d.CLI) > 0 {
		cli, _ := cur["cli"].(map[string]any)
		name, _ := cli["name"].(string)
		if name == "" {
			id, _ := cur["id"].(string)
			name = strings.ReplaceAll(id[strings.LastIndexByte(id, '.')+1:], "-", "")
		}
		if err := section("cli", map[string]any{"name": name, "commands": d.CLI}); err != nil {
			return err
		}
	}
	if len(d.Chat) > 0 {
		var cmds []map[string]any
		for _, c := range d.Chat {
			m := map[string]any{"name": c.Name}
			if len(c.Aliases) > 0 {
				m["aliases"] = c.Aliases
			}
			if c.Capability != nil {
				m["capability"] = *c.Capability
			}
			cd := map[string]string{}
			if c.Cooldown != nil {
				cd["global"] = *c.Cooldown
			}
			if c.UserCooldown != nil {
				cd["user"] = *c.UserCooldown
			}
			if len(cd) > 0 {
				m["cooldown"] = cd
			}
			cmds = append(cmds, m)
		}
		if err := section("chat", map[string]any{"commands": cmds}); err != nil {
			return err
		}
	}
	if len(d.Config) > 0 {
		if err := section("config", d.Config); err != nil {
			return err
		}
	}
	if len(d.Links.Provides)+len(d.Links.Uses) > 0 {
		if err := section("links", d.Links); err != nil {
			return err
		}
	}
	if err := os.WriteFile(path+".tmp", b.Bytes(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	if _, err := capsule.LoadManifest(dir); err != nil {
		return err
	}
	_, err = plugin.Load(dir)
	return err
}

func covers(pattern, typ string) bool {
	if p, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(typ, p)
	}
	return pattern == typ
}

func anyStrings(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, e := range l {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// capsuleDev runs a plugin folder on the daemon against the simulator, regenerating its
// manifest and restarting it on every save, and prints its log until Ctrl-C.
func capsuleDev(p paths, args []string) error {
	fs := flag.NewFlagSet("dev", flag.ContinueOnError)
	live := fs.Bool("live", false, "let chat reach Twitch")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := fs.Arg(0)
	if dir == "" {
		dir = "."
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	var id string
	deploy := func() error {
		if err := generateManifest(p, dir); err != nil {
			return err
		}
		if err := capsule.WriteLock(dir); err != nil {
			return err
		}
		u, err := capsule.AddToStore(p.capsuleStore(), dir)
		if err != nil {
			return err
		}
		id = u.Manifest.ID
		mode := "sim"
		if *live {
			mode = "live"
		}
		return call(p, request{Cmd: "capsule", Args: []string{"dev", u.Hash, mode}})
	}
	if err := deploy(); err != nil {
		return err
	}
	if *live {
		fmt.Println("--live: chat from this plugin reaches Twitch")
	} else {
		fmt.Println("dev mode: chat goes to the daemon's terminal, not Twitch. Try: luna sim follow")
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	if err := w.Add(dir); err != nil {
		return err
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer callOut(p, request{Cmd: "capsule", Args: []string{"undev", id}})
	seen := 0
	poll := time.NewTicker(500 * time.Millisecond) // shortcut: the control socket can't stream yet, so dev polls logs
	defer poll.Stop()
	var settle <-chan time.Time
	for {
		select {
		case <-stop:
			fmt.Println("\nstopped")
			return nil
		case ev := <-w.Events:
			base := filepath.Base(ev.Name)
			if base == capsule.LockFile || base == "plugin.toml" || base == "plugin.toml.tmp" || strings.HasPrefix(base, ".") {
				continue
			}
			settle = time.After(300 * time.Millisecond)
		case <-settle:
			fmt.Println("── change detected, reloading")
			if err := deploy(); err != nil {
				fmt.Fprintln(os.Stderr, "luna:", err)
			}
		case <-poll.C:
			out, err := callOut(p, request{Cmd: "capsule", Args: []string{"logs", id, "--after", strconv.Itoa(seen)}})
			if err != nil {
				continue
			}
			head, rest, _ := strings.Cut(out, "\n")
			if n, err := strconv.Atoi(head); err == nil {
				seen = n
			}
			if rest != "" {
				fmt.Println(rest)
			}
		}
	}
}
