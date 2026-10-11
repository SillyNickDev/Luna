// Package avatar is luna.avatar: channel point redeems and bits drive VRChat avatar
// parameters over OSC, from plain config, and chosen chat shows in the VRChat chatbox.
package avatar

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	luna "luna/sdk/go"
)

//go:embed plugin.toml
var Manifest []byte

// Mapping is one [[plugins.avatar.redeem]] or [[plugins.avatar.bits]] entry.
type Mapping struct {
	Reward   string `json:"reward"` // redeem: the reward's title
	Min      int    `json:"min"`    // bits: the smallest cheer that triggers it
	Param    string `json:"param"`
	Action   string `json:"action"`   // toggle, pulse or set
	Value    any    `json:"value"`    // set and pulse: the value (default true)
	Duration string `json:"duration"` // pulse: how long before it goes back (default 5s)
	Avatar   string `json:"avatar"`   // only on this avatar (ID or name); empty for any
	Off      any    `json:"off"`      // pulse: the value it goes back to (default false or 0)
}

type Settings struct {
	Redeem        []Mapping `luna:"redeem"`
	Bits          []Mapping `luna:"bits"`
	Chatbox       string    `luna:"chatbox"`        // what goes to the VRChat chatbox: off, subs, highlighted, say
	ChatboxReward string    `luna:"chatbox_reward"` // with chatbox = "say": the redeem whose text is shown
	ChatboxFormat string    `luna:"chatbox_format"`
}

type avatar struct {
	bot *luna.Bot
	cfg Settings

	mu    sync.Mutex
	state map[string]any // what Luna last set each parameter to, for toggles VRChat hasn't reported
}

func New() *luna.Bot {
	bot := luna.New()
	a := &avatar{bot: bot, state: map[string]any{}}
	bot.UseConfig(&a.cfg)
	bot.OnStart(func(ctx *luna.Ctx) error {
		for _, p := range a.problems(ctx) {
			bot.Log.Warn(ctx, p)
		}
		return nil
	})
	bot.On("twitch.channel.redeem", func(ctx *luna.Ctx, ev *luna.Event) error {
		reward := ev.Str("reward")
		if a.cfg.Chatbox == "say" && strings.EqualFold(reward, a.cfg.ChatboxReward) {
			if err := a.chatbox(ctx, ev.User(), ev.Str("input")); err != nil {
				return err
			}
		}
		for _, m := range a.cfg.Redeem {
			if strings.EqualFold(m.Reward, reward) {
				if err := a.run(ctx, m); err != nil {
					return err
				}
			}
		}
		return nil
	})
	bot.On("twitch.channel.cheer", func(ctx *luna.Ctx, ev *luna.Event) error {
		// The biggest threshold the cheer reaches wins, so 500 bits doesn't also fire the 100-bit mapping.
		bits := int(ev.Int("bits"))
		best := -1
		for i, m := range a.cfg.Bits {
			if bits >= m.Min && (best < 0 || m.Min > a.cfg.Bits[best].Min) {
				best = i
			}
		}
		if best < 0 {
			return nil
		}
		for _, m := range a.cfg.Bits {
			if m.Min == a.cfg.Bits[best].Min {
				if err := a.run(ctx, m); err != nil {
					return err
				}
			}
		}
		return nil
	})
	bot.On("twitch.chat.message", func(ctx *luna.Ctx, ev *luna.Event) error {
		u := ev.User()
		text := ev.Str("text")
		switch {
		case u == nil || strings.HasPrefix(text, "!"):
			return nil
		case a.cfg.Chatbox == "subs" && u.Has("sub"), a.cfg.Chatbox == "highlighted" && ev.Bool("highlighted"):
			return a.chatbox(ctx, u, text)
		}
		return nil
	})
	bot.On("osc.avatar", func(ctx *luna.Ctx, ev *luna.Event) error {
		a.mu.Lock()
		clear(a.state) // a new avatar starts from its own defaults
		a.mu.Unlock()
		for _, p := range a.problems(ctx) {
			bot.Log.Info(ctx, p)
		}
		return nil
	})
	bot.CLI(luna.CLICommand{Name: "params", Summary: "the current avatar's parameters, and mappings it doesn't have"},
		func(ctx *luna.Ctx, _ luna.Args) (any, error) {
			av, err := bot.OSC.Avatar(ctx)
			if err != nil {
				return nil, err
			}
			used := map[string]bool{}
			for _, m := range a.mappings() {
				used[m.Param] = true
			}
			rows := []map[string]any{}
			for _, p := range av.Params {
				rows = append(rows, map[string]any{"param": p.Name, "type": p.Type, "settable": p.Writable, "mapped": used[p.Name]})
			}
			return map[string]any{"avatar": strings.TrimSpace(av.Name + " " + av.ID), "params": rows, "problems": a.problems(ctx)}, nil
		})
	bot.CLI(luna.CLICommand{Name: "test", Summary: "run a redeem's mappings as if it had been redeemed",
		Args: []luna.Arg{{Name: "reward", Type: "str"}}, Mutates: true},
		func(ctx *luna.Ctx, args luna.Args) (any, error) {
			n := 0
			for _, m := range a.cfg.Redeem {
				if strings.EqualFold(m.Reward, args.Str("reward")) {
					if err := a.run(ctx, m); err != nil {
						return nil, err
					}
					n++
				}
			}
			if n == 0 {
				return nil, fmt.Errorf("no [[plugins.avatar.redeem]] has reward = %q", args.Str("reward"))
			}
			return fmt.Sprintf("ran %d mapping(s)", n), nil
		})
	return bot
}

// Main runs the plugin as a capsule: `luna __avatar` inside the sandbox.
func Main() { New().Run() }

func (a *avatar) mappings() []Mapping { return append(slices.Clone(a.cfg.Redeem), a.cfg.Bits...) }

// problems lists config mistakes, checked against the current avatar's parameters when known.
func (a *avatar) problems(ctx *luna.Ctx) []string {
	out := []string{}
	av, err := a.bot.OSC.Avatar(ctx)
	for _, m := range a.mappings() {
		what := fmt.Sprintf("mapping %q", m.Reward)
		if m.Reward == "" {
			what = fmt.Sprintf("bits mapping (min %d)", m.Min)
		}
		switch {
		case m.Param == "":
			out = append(out, what+": no param")
		case !slices.Contains([]string{"toggle", "pulse", "set"}, m.Action):
			out = append(out, fmt.Sprintf("%s: action %q isn't toggle, pulse or set", what, m.Action))
		case m.Action == "set" && m.Value == nil:
			out = append(out, what+": set needs a value")
		case m.Duration != "":
			if _, err := time.ParseDuration(m.Duration); err != nil {
				out = append(out, fmt.Sprintf("%s: duration %q: %v", what, m.Duration, err))
			}
		}
		if err == nil && len(av.Params) > 0 && a.forAvatar(m, av) && !av.Has(m.Param) {
			out = append(out, fmt.Sprintf("%s: the current avatar has no settable parameter %q", what, m.Param))
		}
	}
	return out
}

func (a *avatar) forAvatar(m Mapping, av *luna.Avatar) bool {
	return m.Avatar == "" || m.Avatar == av.ID || strings.EqualFold(m.Avatar, av.Name)
}

// run applies one mapping, unless the avatar being worn doesn't have the parameter.
func (a *avatar) run(ctx *luna.Ctx, m Mapping) error {
	av, err := a.bot.OSC.Avatar(ctx)
	if err != nil {
		return err
	}
	if !a.forAvatar(m, av) || len(av.Params) > 0 && !av.Has(m.Param) {
		return nil // a different avatar: its parameters may mean something else, or nothing
	}
	on := m.Value
	if on == nil {
		on = true
	}
	switch m.Action {
	case "toggle":
		cur, _ := a.bot.OSC.Param(ctx, m.Param)
		if cur == nil {
			a.mu.Lock()
			cur = a.state[m.Param]
			a.mu.Unlock()
		}
		b, _ := cur.(bool)
		return a.set(ctx, m.Param, !b)
	case "set":
		return a.set(ctx, m.Param, on)
	case "pulse":
		d, err := time.ParseDuration(m.Duration)
		if err != nil || d <= 0 {
			d = 5 * time.Second
		}
		off := m.Off
		if off == nil {
			off = false
			if _, num := on.(float64); num {
				off = 0
			}
		}
		if err := a.set(ctx, m.Param, on); err != nil {
			return err
		}
		a.bot.Timers.After(ctx, d, func(ctx *luna.Ctx) error { return a.set(ctx, m.Param, off) })
	}
	return nil
}

func (a *avatar) set(ctx *luna.Ctx, param string, v any) error {
	if f, ok := v.(float64); ok {
		v = json.Number(strconv.FormatFloat(f, 'f', -1, 64)) // untyped: Luna picks Int or Float from the avatar
	}
	a.mu.Lock()
	a.state[param] = v
	a.mu.Unlock()
	return a.bot.OSC.SetParam(ctx, param, v)
}

func (a *avatar) chatbox(ctx *luna.Ctx, u *luna.User, text string) error {
	text = strings.TrimSpace(text)
	if text == "" || u == nil {
		return nil
	}
	return a.bot.OSC.Chatbox(ctx, strings.NewReplacer("{user}", u.DisplayName(), "{text}", text).Replace(a.cfg.ChatboxFormat))
}
