package bundle

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	luna "luna/sdk/go"
)

type essentials struct {
	builtin   []string             // the bundle's own chat commands; custom ones can't take these names
	cmdLast   map[string]time.Time // custom command cooldowns
	timerLast map[string]time.Time
	timerMsgs map[string]int64 // c.messages when each timer last posted
	bdayDone  map[string]string
}

// customCmd is a streamer-made command: `!cmd add !discord Join us at ...`.
type customCmd struct {
	Response  string `json:"response"`
	Mode      string `json:"mode"` // say, reply, announce, whisper
	CooldownS int    `json:"cooldown_s"`
	Rank      string `json:"rank"` // lowest rank that may use it
	Count     int64  `json:"count"`
}

type chatTimer struct {
	Messages   []string `json:"messages"` // rotated, so chat never sees the same one twice in a row
	IntervalM  int      `json:"interval_m"`
	Next       int      `json:"next"`
	OfflineToo bool     `json:"offline_too"`
}

type counterDef struct {
	PerGame bool `json:"per_game"`
}

type quote struct {
	Text string    `json:"text"`
	Game string    `json:"game"`
	At   time.Time `json:"at"`
	By   string    `json:"by"`
}

type note struct {
	Text string    `json:"text"`
	By   string    `json:"by"`
	At   time.Time `json:"at"`
}

var cmdNameRe = regexp.MustCompile(`^[a-z0-9_]{1,25}$`)

// chat registers one of the bundle's chat commands. The manifest declares the same
// capability and cooldowns; a test keeps the two in step.
func (c *core) chat(cmd luna.ChatCommand, fn func(*luna.Ctx, []string) error) {
	c.builtin = append(c.builtin, cmd.Name)
	c.builtin = append(c.builtin, cmd.Aliases...)
	c.bot.ChatCommand(cmd, fn)
}

// say sends text the way a custom command asked: as a message, reply, announcement or whisper.
func (c *core) say(ctx *luna.Ctx, mode, text string) error {
	switch mode {
	case "reply":
		return ctx.Reply(text)
	case "announce":
		return c.bot.Chat.Announce(ctx, text)
	case "whisper":
		if u := ctx.User(); u != nil {
			return c.bot.Chat.Whisper(ctx, u.ID, text)
		}
	}
	return c.bot.Chat.Send(ctx, text)
}

func isMod(ctx *luna.Ctx) bool { return ctx.User().IsMod() }

func (c *core) setupEssentials() {
	c.cmdLast, c.timerLast, c.timerMsgs, c.bdayDone = map[string]time.Time{}, map[string]time.Time{}, map[string]int64{}, map[string]string{}
	bot := c.bot

	c.chat(luna.ChatCommand{Name: "cmd", Capability: "commands.edit"}, c.cmdCommand)
	c.chat(luna.ChatCommand{Name: "timer", Capability: "commands.edit"}, c.timerCommand)
	c.chat(luna.ChatCommand{Name: "counter", Capability: "commands.edit"}, c.counterCommand)
	c.chat(luna.ChatCommand{Name: "quote", UserCooldown: "5s"}, c.quoteCommand)

	c.chat(luna.ChatCommand{Name: "uptime", Cooldown: "5s"}, func(ctx *luna.Ctx, _ []string) error {
		s, err := bot.Twitch.Stream(ctx, "")
		if err != nil {
			return err
		}
		if !s.Live {
			return ctx.Reply("The stream is offline.")
		}
		return ctx.Reply("Live for " + human(bot.Now().Sub(s.StartedAt)) + ".")
	})
	c.chat(luna.ChatCommand{Name: "game", Cooldown: "5s"}, func(ctx *luna.Ctx, _ []string) error {
		ch, err := bot.Twitch.Channel(ctx, "")
		if err != nil || ch == nil {
			return err
		}
		return ctx.Reply("Playing " + ch.Game + ".")
	})
	c.chat(luna.ChatCommand{Name: "title", Cooldown: "5s"}, func(ctx *luna.Ctx, _ []string) error {
		ch, err := bot.Twitch.Channel(ctx, "")
		if err != nil || ch == nil {
			return err
		}
		return ctx.Reply(ch.Title)
	})
	c.chat(luna.ChatCommand{Name: "followage", UserCooldown: "10s"}, func(ctx *luna.Ctx, args []string) error {
		id, name, err := c.target(ctx, args)
		if err != nil || id == "" {
			return err
		}
		at, err := bot.Twitch.FollowedAt(ctx, id)
		if err != nil {
			return err
		}
		if at.IsZero() {
			return ctx.Reply(name + " doesn't follow the channel.")
		}
		return ctx.Reply(fmt.Sprintf("%s has followed for %s.", name, human(bot.Now().Sub(at))))
	})
	c.chat(luna.ChatCommand{Name: "accountage", UserCooldown: "10s"}, func(ctx *luna.Ctx, args []string) error {
		who := ctx.User().Login
		if len(args) > 0 {
			who = login(args[0])
		}
		u, err := bot.Twitch.User(ctx, who)
		if err != nil {
			return err
		}
		if u == nil {
			return ctx.Reply("No Twitch user " + who + ".")
		}
		return ctx.Reply(fmt.Sprintf("%s's account is %s old.", u.Name, human(bot.Now().Sub(u.CreatedAt))))
	})
	c.chat(luna.ChatCommand{Name: "watchtime", UserCooldown: "10s"}, func(ctx *luna.Ctx, args []string) error {
		who, name := ctx.User().ID, ctx.User().DisplayName()
		if len(args) > 0 {
			who, name = login(args[0]), login(args[0])
		}
		v, err := bot.Viewers.Get(ctx, who)
		if err != nil {
			return err
		}
		if v == nil {
			return ctx.Reply("I haven't seen " + name + " in chat yet.")
		}
		return ctx.Reply(fmt.Sprintf("%s has watched for %s over %s.", name, human(time.Duration(v.WatchS)*time.Second), plural(v.Streams, "stream")))
	})
	c.chat(luna.ChatCommand{Name: "lurk"}, func(ctx *luna.Ctx, _ []string) error {
		if err := bot.Store.Set(ctx, "lurk:"+ctx.User().ID, bot.Now(), 24*time.Hour); err != nil {
			return err
		}
		return ctx.Reply(render(c.cfg.LurkMessage, map[string]string{"user": ctx.User().DisplayName()}))
	})
	c.chat(luna.ChatCommand{Name: "unlurk"}, func(ctx *luna.Ctx, _ []string) error {
		var since time.Time
		if ok, err := bot.Store.Get(ctx, "lurk:"+ctx.User().ID, &since); err != nil || !ok {
			return err
		}
		bot.Store.Delete(ctx, "lurk:"+ctx.User().ID)
		return ctx.Reply(render(c.cfg.UnlurkMessage, map[string]string{"user": ctx.User().DisplayName(), "away": human(bot.Now().Sub(since))}))
	})
	c.chat(luna.ChatCommand{Name: "commands", Cooldown: "30s"}, func(ctx *luna.Ctx, _ []string) error {
		if c.cfg.CommandsURL != "" {
			return ctx.Reply("Commands: " + c.cfg.CommandsURL)
		}
		keys, err := bot.Store.Keys(ctx, "cmd:")
		if err != nil {
			return err
		}
		names := []string{"!uptime", "!game", "!followage", "!watchtime", "!quote", "!lurk"}
		for _, k := range keys {
			names = append(names, "!"+strings.TrimPrefix(k, "cmd:"))
		}
		line := "Commands: " + strings.Join(names, " ")
		if len(line) > 300 { // a link beats a wall of chat; this is the fallback without one
			line = line[:strings.LastIndexByte(line[:300], ' ')] + " …"
		}
		return ctx.Reply(line)
	})
	c.chat(luna.ChatCommand{Name: "birthday", UserCooldown: "10s"}, func(ctx *luna.Ctx, args []string) error {
		if len(args) == 1 && args[0] == "clear" {
			return bot.Store.Delete(ctx, "bday:"+ctx.User().ID)
		}
		d, err := time.Parse("01-02", strings.Join(args, ""))
		if len(args) != 1 || err != nil {
			return ctx.Reply("Usage: !birthday MM-DD (just the day, never the year), or !birthday clear")
		}
		if err := bot.Store.Set(ctx, "bday:"+ctx.User().ID, d.Format("01-02"), 0); err != nil {
			return err
		}
		return ctx.Reply("Got it, " + d.Format("January 2") + ".")
	})
	c.chat(luna.ChatCommand{Name: "note", Capability: "mod.notes"}, func(ctx *luna.Ctx, args []string) error {
		if len(args) < 2 {
			return ctx.Reply("Usage: !note <user> <text> (mods only; shown in !mod history)")
		}
		who := login(args[0])
		var notes []note
		if _, err := bot.Store.Get(ctx, "notes:"+who, &notes); err != nil {
			return err
		}
		notes = append(notes, note{Text: strings.Join(args[1:], " "), By: ctx.User().Login, At: bot.Now()})
		if len(notes) > 20 {
			notes = notes[len(notes)-20:]
		}
		if err := bot.Store.Set(ctx, "notes:"+who, notes, 0); err != nil {
			return err
		}
		return ctx.Reply("Noted for " + who + ".")
	})

	bot.Timers.Every(time.Minute, c.runTimers)

	bot.CLI(luna.CLICommand{Name: "commands", Summary: "custom chat commands"}, func(ctx *luna.Ctx, _ luna.Args) (any, error) {
		return list(c, ctx, "cmd:", func(k string, raw customCmd) map[string]any {
			return map[string]any{"name": "!" + k, "response": raw.Response, "mode": raw.Mode, "rank": raw.Rank, "cooldown_s": raw.CooldownS, "uses": raw.Count}
		})
	})
	bot.CLI(luna.CLICommand{Name: "quotes", Summary: "every quote"}, func(ctx *luna.Ctx, _ luna.Args) (any, error) {
		return list(c, ctx, "quote:", func(k string, q quote) map[string]any {
			n, _ := strconv.Atoi(k)
			return map[string]any{"n": n, "text": q.Text, "game": q.Game, "date": q.At.Format("2006-01-02")}
		})
	})
	bot.CLI(luna.CLICommand{Name: "counters", Summary: "counters and their values"}, func(ctx *luna.Ctx, _ luna.Args) (any, error) {
		return list(c, ctx, "count:", func(k string, n int64) map[string]any {
			name, game, _ := strings.Cut(k, "@")
			return map[string]any{"counter": name, "game": game, "value": n}
		})
	})
	bot.CLI(luna.CLICommand{Name: "timers", Summary: "chat timers"}, func(ctx *luna.Ctx, _ luna.Args) (any, error) {
		return list(c, ctx, "timer:", func(k string, t chatTimer) map[string]any {
			return map[string]any{"name": k, "every_m": t.IntervalM, "messages": len(t.Messages), "offline_too": t.OfflineToo}
		})
	})
}

// list reads every key under prefix into rows for a CLI table.
func list[T any](c *core, ctx *luna.Ctx, prefix string, row func(string, T) map[string]any) ([]map[string]any, error) {
	keys, err := c.bot.Store.Keys(ctx, prefix)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, k := range keys {
		var v T
		if ok, err := c.bot.Store.Get(ctx, k, &v); err != nil {
			return nil, err
		} else if ok {
			out = append(out, row(strings.TrimPrefix(k, prefix), v))
		}
	}
	return out, nil
}

// target is who a command is about: the first argument, or whoever asked.
func (c *core) target(ctx *luna.Ctx, args []string) (id, name string, err error) {
	if len(args) == 0 {
		return ctx.User().ID, ctx.User().DisplayName(), nil
	}
	u, err := c.bot.Twitch.User(ctx, login(args[0]))
	if err != nil {
		return "", "", err
	}
	if u == nil {
		return "", "", ctx.Reply("No Twitch user " + login(args[0]) + ".")
	}
	return u.ID, u.Name, nil
}

func cmdName(s string) string { return strings.ToLower(strings.TrimPrefix(s, "!")) }

func (c *core) cmdCommand(ctx *luna.Ctx, args []string) error {
	bot := c.bot
	usage := "Usage: !cmd add|edit !name <response> · !cmd remove !name · !cmd alias !new !existing · !cmd set !name mode say|reply|announce|whisper · cooldown <s> · rank viewer|moderator"
	if len(args) < 2 {
		return ctx.Reply(usage)
	}
	verb, name := args[0], cmdName(args[1])
	if !cmdNameRe.MatchString(name) {
		return ctx.Reply("Command names are letters, digits and _, up to 25.")
	}
	if slices.Contains(c.builtin, name) {
		return ctx.Reply("!" + name + " is built in.")
	}
	key := "cmd:" + name
	var cmd customCmd
	exists, err := bot.Store.Get(ctx, key, &cmd)
	if err != nil {
		return err
	}
	switch {
	case verb == "add" && len(args) > 2:
		if exists {
			return ctx.Reply("!" + name + " already exists; use !cmd edit.")
		}
		cmd = customCmd{Response: strings.Join(args[2:], " "), Mode: "say", Rank: "viewer", CooldownS: 5}
	case verb == "edit" && len(args) > 2 && exists:
		cmd.Response = strings.Join(args[2:], " ")
	case verb == "remove" && exists:
		var b luna.Batch
		b.Delete(key)
		aliases, err := bot.Store.Keys(ctx, "alias:")
		if err != nil {
			return err
		}
		for _, a := range aliases {
			var to string
			if ok, _ := bot.Store.Get(ctx, a, &to); ok && to == name {
				b.Delete(a)
			}
		}
		if err := bot.Store.Apply(ctx, &b); err != nil {
			return err
		}
		return ctx.Reply("Removed !" + name + ".")
	case verb == "alias" && len(args) == 3:
		to := cmdName(args[2])
		if ok, err := bot.Store.Get(ctx, "cmd:"+to, &customCmd{}); err != nil || !ok {
			return ctx.Reply("No custom command !" + to + ".")
		}
		if exists {
			return ctx.Reply("!" + name + " is already a command.")
		}
		if err := bot.Store.Set(ctx, "alias:"+name, to, 0); err != nil {
			return err
		}
		return ctx.Reply("!" + name + " now runs !" + to + ".")
	case verb == "set" && len(args) == 4 && exists:
		switch v := args[3]; args[2] {
		case "mode":
			if !slices.Contains([]string{"say", "reply", "announce", "whisper"}, v) {
				return ctx.Reply("Mode is say, reply, announce or whisper.")
			}
			cmd.Mode = v
		case "cooldown":
			n, err := strconv.Atoi(strings.TrimSuffix(v, "s"))
			if err != nil || n < 0 || n > 3600 {
				return ctx.Reply("Cooldown is 0-3600 seconds.")
			}
			cmd.CooldownS = n
		case "rank":
			if !slices.Contains(rankOrder[1:], v) {
				return ctx.Reply("Rank is viewer, moderator, luna-mod, head-mod or broadcaster.")
			}
			cmd.Rank = v
		default:
			return ctx.Reply(usage)
		}
	case !exists && verb != "add":
		return ctx.Reply("No custom command !" + name + ".")
	default:
		return ctx.Reply(usage)
	}
	if len(cmd.Response) > 450 {
		return ctx.Reply("Responses are up to 450 characters.")
	}
	if err := bot.Store.Set(ctx, key, cmd, 0); err != nil {
		return err
	}
	return ctx.Reply("Saved !" + name + ".")
}

// chatExtras handles chat that isn't one of the bundle's declared commands: birthdays,
// custom commands and counters.
func (c *core) chatExtras(ctx *luna.Ctx, ev *luna.Event) error {
	if err := c.birthday(ctx); err != nil {
		return err
	}
	words := strings.Fields(ev.Str("text"))
	if len(words) == 0 || !strings.HasPrefix(words[0], "!") {
		return nil
	}
	name := cmdName(words[0])
	if !cmdNameRe.MatchString(name) || slices.Contains(c.builtin, name) {
		return nil
	}
	var to string
	if ok, err := c.bot.Store.Get(ctx, "alias:"+name, &to); err != nil {
		return err
	} else if ok {
		name = to
	}
	var def counterDef
	if ok, err := c.bot.Store.Get(ctx, "counterdef:"+name, &def); err != nil {
		return err
	} else if ok {
		return c.useCounter(ctx, name, def, words[1:])
	}
	var cmd customCmd
	if ok, err := c.bot.Store.Get(ctx, "cmd:"+name, &cmd); err != nil || !ok {
		return err
	}
	if !ctx.User().RankAtLeast(cmd.Rank) {
		return nil
	}
	c.mu.Lock()
	now := c.bot.Now()
	if now.Sub(c.cmdLast[name]) < time.Duration(cmd.CooldownS)*time.Second {
		c.mu.Unlock()
		return nil
	}
	c.cmdLast[name] = now
	c.mu.Unlock()
	cmd.Count++
	if err := c.bot.Store.Set(ctx, "cmd:"+name, cmd, 0); err != nil {
		return err
	}
	text, err := c.fill(ctx, cmd.Response, words[1:], cmd.Count)
	if err != nil {
		return err
	}
	return c.say(ctx, cmd.Mode, text)
}

// fill renders a custom command's variables, asking Twitch only for the ones it uses.
func (c *core) fill(ctx *luna.Ctx, tmpl string, args []string, count int64) (string, error) {
	u := ctx.User()
	vars := map[string]string{"user": u.DisplayName(), "target": u.DisplayName(), "count": strconv.FormatInt(count, 10)}
	if len(args) > 0 {
		vars["target"] = strings.TrimPrefix(args[0], "@")
	}
	if strings.Contains(tmpl, "{uptime}") {
		vars["uptime"] = "offline"
		if s, err := c.bot.Twitch.Stream(ctx, ""); err != nil {
			return "", err
		} else if s.Live {
			vars["uptime"] = human(c.bot.Now().Sub(s.StartedAt))
		}
	}
	if strings.Contains(tmpl, "{game}") || strings.Contains(tmpl, "{title}") {
		ch, err := c.bot.Twitch.Channel(ctx, "")
		if err != nil {
			return "", err
		}
		if ch != nil {
			vars["game"], vars["title"] = ch.Game, ch.Title
		}
	}
	if strings.Contains(tmpl, "{followage}") {
		vars["followage"] = "not following"
		if at, err := c.bot.Twitch.FollowedAt(ctx, u.ID); err != nil {
			return "", err
		} else if !at.IsZero() {
			vars["followage"] = human(c.bot.Now().Sub(at))
		}
	}
	return render(tmpl, vars), nil
}

func (c *core) timerCommand(ctx *luna.Ctx, args []string) error {
	bot := c.bot
	usage := "Usage: !timer add <name> <minutes> <message> · !timer msg <name> <another message> · !timer offline <name> on|off · !timer remove <name>"
	if len(args) < 2 {
		return ctx.Reply(usage)
	}
	verb, name := args[0], strings.ToLower(args[1])
	if !cmdNameRe.MatchString(name) {
		return ctx.Reply("Timer names are letters, digits and _, up to 25.")
	}
	key := "timer:" + name
	var t chatTimer
	exists, err := bot.Store.Get(ctx, key, &t)
	if err != nil {
		return err
	}
	switch {
	case verb == "add" && len(args) > 3:
		m, err := strconv.Atoi(args[2])
		if err != nil || m < 1 || m > 24*60 {
			return ctx.Reply("Minutes is 1-1440.")
		}
		t = chatTimer{IntervalM: m, Messages: []string{strings.Join(args[3:], " ")}}
	case verb == "msg" && len(args) > 2 && exists:
		if len(t.Messages) >= 10 {
			return ctx.Reply("A timer rotates through up to 10 messages.")
		}
		t.Messages = append(t.Messages, strings.Join(args[2:], " "))
	case verb == "offline" && len(args) == 3 && exists:
		t.OfflineToo = args[2] == "on"
	case verb == "remove" && exists:
		if err := bot.Store.Delete(ctx, key); err != nil {
			return err
		}
		return ctx.Reply("Removed timer " + name + ".")
	case !exists && verb != "add":
		return ctx.Reply("No timer " + name + ".")
	default:
		return ctx.Reply(usage)
	}
	if err := bot.Store.Set(ctx, key, t, 0); err != nil {
		return err
	}
	return ctx.Reply(fmt.Sprintf("Timer %s: every %d min, %s.", name, t.IntervalM, plural(int64(len(t.Messages)), "message")))
}

// runTimers posts each due timer: only while live (unless it says otherwise) and only
// once chat has said timer_min_messages things since its last post.
func (c *core) runTimers(ctx *luna.Ctx) error {
	keys, err := c.bot.Store.Keys(ctx, "timer:")
	if err != nil {
		return err
	}
	now := c.bot.Now()
	for _, k := range keys {
		var t chatTimer
		if ok, err := c.bot.Store.Get(ctx, k, &t); err != nil || !ok || len(t.Messages) == 0 {
			continue
		}
		c.mu.Lock()
		last, seen := c.timerLast[k]
		if !seen { // start the clock at the first tick, so a restart doesn't post everything at once
			c.timerLast[k], c.timerMsgs[k] = now, c.messages
		}
		due := seen && (c.live || t.OfflineToo) && now.Sub(last) >= time.Duration(t.IntervalM)*time.Minute &&
			c.messages-c.timerMsgs[k] >= int64(c.cfg.TimerMinMessages)
		if due {
			c.timerLast[k], c.timerMsgs[k] = now, c.messages
		}
		c.mu.Unlock()
		if !due {
			continue
		}
		msg := t.Messages[t.Next%len(t.Messages)]
		t.Next = (t.Next + 1) % len(t.Messages)
		if err := c.bot.Store.Set(ctx, k, t, 0); err != nil {
			return err
		}
		if err := c.bot.Chat.Send(ctx, msg); err != nil {
			return err
		}
	}
	return nil
}

func (c *core) counterCommand(ctx *luna.Ctx, args []string) error {
	if len(args) < 2 || (args[0] != "add" && args[0] != "remove") {
		return ctx.Reply("Usage: !counter add <name> [pergame] · !counter remove <name>")
	}
	name := cmdName(args[1])
	if !cmdNameRe.MatchString(name) || slices.Contains(c.builtin, name) {
		return ctx.Reply("Pick another name: letters, digits and _, not a built-in command.")
	}
	if args[0] == "remove" {
		if err := c.bot.Store.Delete(ctx, "counterdef:"+name); err != nil {
			return err
		}
		return ctx.Reply("Removed !" + name + " (its counts are kept).")
	}
	def := counterDef{PerGame: len(args) > 2 && args[2] == "pergame"}
	if err := c.bot.Store.Set(ctx, "counterdef:"+name, def, 0); err != nil {
		return err
	}
	how := ""
	if def.PerGame {
		how = ", counted per game"
	}
	return ctx.Reply(fmt.Sprintf("Counter !%s ready%s. Mods: !%s +, !%s -, !%s set <n>", name, how, name, name, name))
}

func (c *core) useCounter(ctx *luna.Ctx, name string, def counterDef, args []string) error {
	key, label := "count:"+name, name
	if g := c.currentGame(); def.PerGame && g != "" {
		key, label = key+"@"+strings.ToLower(g), name+" in "+g
	}
	var n int64
	if _, err := c.bot.Store.Get(ctx, key, &n); err != nil {
		return err
	}
	if len(args) > 0 && isMod(ctx) {
		prev := n
		switch {
		case args[0] == "+":
			n++
		case args[0] == "-":
			n--
		case args[0] == "set" && len(args) == 2:
			v, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return ctx.Reply("Usage: !" + name + " set <number>")
			}
			n = v
		default:
			return ctx.Reply("Usage: !" + name + " + · - · set <number>")
		}
		var b luna.Batch
		b.Set(key, n, 0)
		if err := c.bot.Store.Apply(ctx, &b); err != nil {
			return err
		}
		if _, err := c.record(ctx, entry{Action: "counter", Detail: fmt.Sprintf("%s %d → %d", label, prev, n),
			Undo: &undo{Counter: key, Value: prev}}); err != nil {
			return err
		}
	}
	return c.bot.Chat.Send(ctx, fmt.Sprintf("%s: %d", strings.ToUpper(label[:1])+label[1:], n))
}

func (c *core) quoteCommand(ctx *luna.Ctx, args []string) error {
	bot := c.bot
	if len(args) > 1 && args[0] == "add" && isMod(ctx) {
		text := strings.Join(args[1:], " ")
		if len(text) > 400 {
			return ctx.Reply("Quotes are up to 400 characters.")
		}
		n, err := bot.Store.Incr(ctx, "quote.seq", 1)
		if err != nil {
			return err
		}
		if err := bot.Store.Set(ctx, fmt.Sprintf("quote:%d", n), quote{Text: text, Game: c.currentGame(), At: bot.Now(), By: ctx.User().Login}, 0); err != nil {
			return err
		}
		return ctx.Reply(fmt.Sprintf("Added quote #%d.", n))
	}
	if len(args) == 2 && args[0] == "remove" && isMod(ctx) {
		if err := bot.Store.Delete(ctx, "quote:"+args[1]); err != nil {
			return err
		}
		return ctx.Reply("Removed quote #" + args[1] + ".")
	}
	keys, err := bot.Store.Keys(ctx, "quote:")
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return ctx.Reply("No quotes yet. Mods: !quote add <text>")
	}
	pick := keys[rand.IntN(len(keys))]
	switch {
	case len(args) == 1:
		if _, err := strconv.Atoi(args[0]); err != nil {
			return ctx.Reply("Usage: !quote · !quote <n> · !quote search <word>")
		}
		pick = "quote:" + args[0]
	case len(args) > 1 && args[0] == "search":
		want := fold(strings.Join(args[1:], " "))
		pick = ""
		for _, k := range keys {
			var q quote
			if ok, _ := bot.Store.Get(ctx, k, &q); ok && strings.Contains(fold(q.Text), want) {
				pick = k
				break
			}
		}
		if pick == "" {
			return ctx.Reply("No quote mentions that.")
		}
	}
	var q quote
	if ok, err := bot.Store.Get(ctx, pick, &q); err != nil {
		return err
	} else if !ok {
		return ctx.Reply("No such quote.")
	}
	where := ""
	if q.Game != "" {
		where = " [" + q.Game + "]"
	}
	return ctx.Reply(fmt.Sprintf("#%s: %s%s %s", strings.TrimPrefix(pick, "quote:"), q.Text, where, q.At.Format("2006-01-02")))
}

// birthday wishes a viewer the first time they chat on their day (UTC).
func (c *core) birthday(ctx *luna.Ctx) error {
	u := ctx.User()
	today := c.bot.Now().Format("01-02")
	year := c.bot.Now().Format("2006")
	c.mu.Lock()
	done := c.bdayDone[u.ID] == today
	if len(c.bdayDone) > 10000 {
		c.bdayDone = map[string]string{}
	}
	c.bdayDone[u.ID] = today // one store read per viewer per day
	c.mu.Unlock()
	if done {
		return nil
	}
	var day string
	if ok, err := c.bot.Store.Get(ctx, "bday:"+u.ID, &day); err != nil || !ok || day != today {
		return err
	}
	var wished string
	if _, err := c.bot.Store.Get(ctx, "bdaywished:"+u.ID, &wished); err != nil || wished == year {
		return err
	}
	if err := c.bot.Store.Set(ctx, "bdaywished:"+u.ID, year, 0); err != nil {
		return err
	}
	return c.bot.Chat.Send(ctx, render(c.cfg.BirthdayMessage, map[string]string{"user": u.DisplayName()}))
}

// isRegular is a viewer past regular_watch_hours of watch time.
func (c *core) isRegular(ctx *luna.Ctx, userID string) (bool, error) {
	v, err := c.bot.Viewers.Get(ctx, userID)
	if err != nil || v == nil {
		return false, err
	}
	return v.WatchS >= int64(c.cfg.RegularWatchHours)*3600, nil
}
