package lunatest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"luna/internal/capsule"
	"luna/internal/plugin"
	luna "luna/sdk/go"
)

// CheckManifest fails tb where dir/plugin.toml and the bot's code disagree: chat and CLI
// commands, config, events and links. The harness routes by the code, Luna by plugin.toml.
func CheckManifest(tb testing.TB, bot *luna.Bot, dir string) {
	tb.Helper()
	cm, err := capsule.LoadManifest(dir)
	if err != nil {
		tb.Fatal(err)
	}
	m, err := plugin.Load(dir)
	if err != nil {
		tb.Fatal(err)
	}
	d := bot.Describe()
	var code []map[string]any
	b, _ := json.Marshal(d["chat"])
	json.Unmarshal(b, &code)
	if len(code) != len(m.Chat.Commands) {
		tb.Fatalf("code has %d chat commands, plugin.toml %d", len(code), len(m.Chat.Commands))
	}
	for _, c := range code {
		i := slices.IndexFunc(m.Chat.Commands, func(mc plugin.ChatCommand) bool { return mc.Name == c["name"] })
		if i < 0 {
			tb.Fatalf("!%s isn't in plugin.toml", c["name"])
		}
		mc := m.Chat.Commands[i]
		capab, _ := c["capability"].(string)
		cd, _ := c["cooldown"].(string)
		ucd, _ := c["user_cooldown"].(string)
		var aliases []string
		for _, a := range c["aliases"].([]any) {
			aliases = append(aliases, a.(string))
		}
		pd := func(s string) time.Duration { d, _ := time.ParseDuration(s); return d }
		if capab != mc.Capability || pd(cd) != time.Duration(mc.Cooldown.Global) || pd(ucd) != time.Duration(mc.Cooldown.User) ||
			!slices.Equal(aliases, mc.Aliases) {
			tb.Errorf("!%s: code %v, plugin.toml %+v", c["name"], c, mc)
		}
	}
	var cli []plugin.CLICommand
	b, _ = json.Marshal(d["cli"])
	json.Unmarshal(b, &cli)
	slices.SortFunc(m.CLI.Commands, func(a, b plugin.CLICommand) int { return strings.Compare(a.Name, b.Name) })
	if len(cli)+len(m.CLI.Commands) > 0 && !reflect.DeepEqual(cli, m.CLI.Commands) {
		tb.Errorf("CLI commands differ:\ncode %+v\ntoml %+v", cli, m.CLI.Commands)
	}
	src, err := os.ReadFile(filepath.Join(dir, "plugin.toml"))
	if err != nil {
		tb.Fatal(err)
	}
	var raw struct {
		Config map[string]map[string]any `toml:"config"`
	}
	toml.Decode(string(src), &raw)
	cfg := d["config"].(map[string]map[string]any)
	for k, f := range raw.Config {
		if cfg[k] == nil || cfg[k]["type"] != f["type"] {
			tb.Errorf("config %s: plugin.toml %v, Settings %v", k, f, cfg[k])
		}
	}
	if len(cfg) != len(raw.Config) {
		tb.Errorf("Settings has %d fields, plugin.toml [config] %d", len(cfg), len(raw.Config))
	}
	var events []string
	for _, e := range d["events"].([]string) {
		if !m.AllowsEvents(e) {
			tb.Errorf("handles %s without [permissions].events", e)
		}
		events = append(events, e)
	}
	if len(events) != len(m.Permissions.Events) {
		tb.Errorf("asks for events it doesn't handle: %v vs %v", m.Permissions.Events, events)
	}
	links := d["links"].(map[string][]string)
	if !slices.Equal(links["provides"], slices.Sorted(slices.Values(cm.Links.Provides))) ||
		!slices.Equal(links["uses"], slices.Sorted(slices.Values(cm.Links.Uses))) {
		tb.Errorf("links: code %v, plugin.toml %+v", links, cm.Links)
	}
}
