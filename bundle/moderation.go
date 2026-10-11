package bundle

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	luna "luna/sdk/go"
)

type moderation struct {
	recent      []seenMsg            // the last few minutes of chat, for !nuke
	byUser      map[string][]seenMsg // recent messages per user, for the repeat filter
	follows     []time.Time          // recent follows, for follow-bot waves
	firstTexts  map[string][]time.Time
	shieldOn    bool
	shieldOff   func() // cancels the automatic turn-off
	raidGrace   time.Time
	phrases     []*regexp.Regexp
	phrasesRead bool
}

type seenMsg struct {
	userID, login, folded, msgID string
	mod                          bool
	at                           time.Time
}

type strike struct {
	N    int       `json:"n"`
	Last time.Time `json:"last"`
}

// linkRe finds URLs and bare domains on common TLDs ("example.com/x"), not "e.g." or "lol.ok".
var linkRe = regexp.MustCompile(`(?i)(?:https?://([^\s/]+)|\b((?:[a-z0-9-]+\.)+(?:com|net|org|tv|gg|io|ly|me|co|uk|de|xyz|info|biz|ru|app|dev|link|live|site|online|shop|store|club|fun|top|be|fm|to|cc|us|ca|eu|fr|nl|es|it|jp|br|au|in|pl|ws|sh|gl|gd|ai|so|pw|tk|ml|ga|cf|gq|ly|bit)\b))`)

// links returns the hosts in text that aren't on the allowlist.
func links(text string, allow []string) []string {
	var out []string
	for _, m := range linkRe.FindAllStringSubmatch(text, -1) {
		host := strings.ToLower(m[1] + m[2])
		if !slices.ContainsFunc(allow, func(d string) bool { return host == d || strings.HasSuffix(host, "."+d) }) {
			out = append(out, host)
		}
	}
	return out
}

// sameText reduces a message to what makes it "the same" as another: folded, no spaces,
// runs of one character collapsed to one.
func sameText(s string) string {
	var b strings.Builder
	var last rune
	for _, r := range fold(s) {
		if !unicode.IsSpace(r) && r != last {
			b.WriteRune(r)
		}
		if !unicode.IsSpace(r) {
			last = r
		}
	}
	return b.String()
}

// filter returns why text breaks a filter, or "".
func (c *core) filter(ctx *luna.Ctx, u *luna.User, text string, emotes int64) (string, error) {
	cfg := c.cfg
	if cfg.ModLinks {
		if bad := links(text, csv(cfg.ModLinkAllow)); len(bad) > 0 {
			var permitted bool
			if ok, err := c.bot.Store.Get(ctx, "permit:"+u.Login, &permitted); err != nil {
				return "", err
			} else if !ok {
				return "links", nil
			}
			if err := c.bot.Store.Delete(ctx, "permit:"+u.Login); err != nil { // a permit is good for one message
				return "", err
			}
		}
	}
	folded := fold(text)
	if cfg.ModPhrases {
		res, err := c.bannedPhrases(ctx)
		if err != nil {
			return "", err
		}
		for _, re := range res {
			if re.MatchString(folded) {
				return "banned phrase", nil
			}
		}
	}
	runes := []rune(text)
	if cfg.ModCaps && len(runes) > cfg.ModCapsMinLen {
		letters, upper := 0, 0
		for _, r := range runes {
			if unicode.IsLetter(r) {
				letters++
				if unicode.IsUpper(r) {
					upper++
				}
			}
		}
		if letters > 0 && upper*100/letters > cfg.ModCapsPercent {
			return "caps", nil
		}
	}
	if cfg.ModSymbols {
		marks, symbols := 0, 0
		for _, r := range runes {
			switch {
			case unicode.Is(unicode.Mn, r):
				marks++
			case !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsSpace(r):
				symbols++
			}
		}
		if marks > 10 || len(runes) >= 15 && symbols*100/len(runes) > 60 {
			return "symbols", nil
		}
	}
	if cfg.ModRepeat && !strings.HasPrefix(text, "!") { // repeating a command is how commands work
		s := sameText(text)
		now := c.bot.Now()
		c.mu.Lock()
		n := 0
		for _, m := range c.byUser[u.ID] {
			if m.folded == s && now.Sub(m.at) < time.Duration(cfg.ModRepeatWindowS)*time.Second {
				n++
			}
		}
		c.mu.Unlock()
		if n+1 >= cfg.ModRepeatCount {
			return "repeated message", nil
		}
	}
	if cfg.ModEmotes && emotes > int64(cfg.ModEmotesMax) {
		return "emote spam", nil
	}
	if cfg.ModLong && len(runes) > cfg.ModLongMax {
		return "long message", nil
	}
	return "", nil
}

func (c *core) bannedPhrases(ctx *luna.Ctx) ([]*regexp.Regexp, error) {
	c.mu.Lock()
	if c.phrasesRead {
		defer c.mu.Unlock()
		return c.phrases, nil
	}
	c.mu.Unlock()
	var pats []string
	if _, err := c.bot.Store.Get(ctx, "phrases", &pats); err != nil {
		return nil, err
	}
	var res []*regexp.Regexp
	for _, p := range pats {
		if re, err := regexp.Compile("(?i)" + p); err == nil {
			res = append(res, re)
		}
	}
	c.mu.Lock()
	c.phrases, c.phrasesRead = res, true
	c.mu.Unlock()
	return res, nil
}

// exempt applies mod_exempt: vip, sub and regular. Mods and up are never filtered.
func (c *core) exempt(ctx *luna.Ctx, u *luna.User) (bool, error) {
	ex := csv(c.cfg.ModExempt)
	if slices.Contains(ex, "vip") && u.Has("vip") || slices.Contains(ex, "sub") && u.Has("sub") {
		return true, nil
	}
	if slices.Contains(ex, "regular") {
		return c.isRegular(ctx, u.ID)
	}
	return false, nil
}

// moderate runs the filters on a chat message and, on a hit, climbs the user's
// escalation ladder. It reports whether the message was removed.
func (c *core) moderate(ctx *luna.Ctx, ev *luna.Event) (bool, error) {
	u := ev.User()
	if u == nil {
		return false, nil
	}
	text, now := ev.Str("text"), c.bot.Now()
	msg := seenMsg{userID: u.ID, login: u.Login, folded: fold(text), msgID: ev.Str("message_id"), mod: u.IsMod(), at: now}
	defer c.remember(msg, sameText(text))
	if ev.Bool("first") {
		if err := c.watchFirstTexts(ctx, sameText(text)); err != nil {
			return false, err
		}
	}
	if msg.mod {
		return false, nil
	}
	reason, err := c.filter(ctx, u, text, ev.Int("emotes"))
	if err != nil || reason == "" {
		return false, err
	}
	if ok, err := c.exempt(ctx, u); err != nil || ok {
		return false, err
	}
	var s strike
	if _, err := c.bot.Store.Get(ctx, "strike:"+u.ID, &s); err != nil {
		return false, err
	}
	if now.Sub(s.Last) > time.Duration(c.cfg.ModStrikeDecayM)*time.Minute {
		s.N = 0
	}
	s.N++
	s.Last = now
	if err := c.bot.Store.Set(ctx, "strike:"+u.ID, s, 7*24*time.Hour); err != nil {
		return false, err
	}
	ladder := csv(c.cfg.ModLadder)
	if len(ladder) == 0 {
		ladder = []string{"delete"}
	}
	step := ladder[min(s.N, len(ladder))-1]
	why := fmt.Sprintf("%s (strike %d)", reason, s.N)
	e := entry{Actor: "luna", Rank: "luna", User: u.Login, UserID: u.ID, Detail: why}
	switch d, err := time.ParseDuration(step); {
	case err == nil && d > 0:
		if err := c.bot.Mod.Timeout(ctx, u.ID, d, why); err != nil {
			return false, err
		}
		e.Action, e.Detail, e.Undo = "timeout", why+" for "+step, &undo{Unban: []string{u.ID}}
	default: // delete, or flag: delete and leave it for a human
		if err := c.bot.Mod.Delete(ctx, msg.msgID); err != nil {
			return false, err
		}
		e.Action = "delete"
		if step == "flag" {
			e.Action = "flag"
			c.bot.Log.Warn(ctx, "flagged for a human: "+u.Login+": "+why)
		} else if c.cfg.ModWarnNative {
			if err := c.bot.Mod.Warn(ctx, u.ID, reason); err != nil {
				return false, err
			}
		}
	}
	_, err = c.record(ctx, e)
	return true, err
}

// remember keeps recent chat for !nuke and the repeat filter, bounded by age and count.
func (c *core) remember(m seenMsg, same string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cut := m.at.Add(-5 * time.Minute)
	i := 0
	for i < len(c.recent) && (c.recent[i].at.Before(cut) || len(c.recent)-i >= 2000) {
		i++
	}
	c.recent = append(c.recent[i:], m)
	if c.byUser == nil || len(c.byUser) > 5000 {
		c.byUser = map[string][]seenMsg{}
	}
	mine := c.byUser[m.userID]
	if len(mine) >= 10 {
		mine = mine[1:]
	}
	m.folded = same
	c.byUser[m.userID] = append(mine, m)
}

// ---------- raid and bot shield ----------

func (c *core) watchFirstTexts(ctx *luna.Ctx, same string) error {
	if !c.cfg.ModShield || same == "" {
		return nil
	}
	now := c.bot.Now()
	c.mu.Lock()
	if c.firstTexts == nil || len(c.firstTexts) > 1000 {
		c.firstTexts = map[string][]time.Time{}
	}
	ts := append(c.firstTexts[same], now)
	ts = slices.DeleteFunc(ts, func(t time.Time) bool { return now.Sub(t) > time.Duration(c.cfg.ModShieldWindowS)*time.Second })
	c.firstTexts[same] = ts
	trip := len(ts) >= c.cfg.ModShieldSame && !c.shieldOn && now.After(c.raidGrace)
	c.mu.Unlock()
	if trip {
		return c.setShield(ctx, true, fmt.Sprintf("%d new chatters sent the same message", len(ts)))
	}
	return nil
}

func (c *core) watchFollows(ctx *luna.Ctx, _ *luna.Event) error {
	if !c.cfg.ModShield {
		return nil
	}
	now := c.bot.Now()
	c.mu.Lock()
	c.follows = append(c.follows, now)
	c.follows = slices.DeleteFunc(c.follows, func(t time.Time) bool { return now.Sub(t) > time.Duration(c.cfg.ModShieldWindowS)*time.Second })
	n := len(c.follows)
	trip := n >= c.cfg.ModShieldFollows && !c.shieldOn && now.After(c.raidGrace)
	c.mu.Unlock()
	if trip {
		return c.setShield(ctx, true, fmt.Sprintf("%d follows in %ds", n, c.cfg.ModShieldWindowS))
	}
	return nil
}

// raided exempts legitimate raids from the shield: from a friend (so_friends) or a
// channel that has raided before.
func (c *core) raided(ctx *luna.Ctx, ev *luna.Event) error {
	u := ev.User()
	if u == nil {
		return nil
	}
	var before bool
	ok, err := c.bot.Store.Get(ctx, "raided_by:"+u.ID, &before)
	if err != nil {
		return err
	}
	if err := c.bot.Store.Set(ctx, "raided_by:"+u.ID, true, 0); err != nil {
		return err
	}
	if ok || slices.Contains(csv(c.cfg.SoFriends), u.Login) {
		c.mu.Lock()
		c.raidGrace = c.bot.Now().Add(5 * time.Minute)
		c.mu.Unlock()
	}
	return nil
}

// setShield turns Twitch's Shield Mode on or off; on, it tells mods how to end it and
// turns itself off after mod_shield_off_m.
func (c *core) setShield(ctx *luna.Ctx, on bool, reason string) error {
	if err := c.bot.Mod.Shield(ctx, on); err != nil {
		return err
	}
	c.mu.Lock()
	c.shieldOn = on
	if c.shieldOff != nil {
		c.shieldOff()
		c.shieldOff = nil
	}
	if on {
		c.shieldOff = c.bot.Timers.After(ctx, time.Duration(c.cfg.ModShieldOffM)*time.Minute, func(ctx *luna.Ctx) error {
			return c.setShield(ctx, false, "")
		})
	}
	c.mu.Unlock()
	if !on {
		return c.bot.Chat.Send(ctx, "🛡️ Shield Mode is off.")
	}
	e := entry{Action: "shield", Detail: reason, Undo: &undo{ShieldO: true}}
	if reason != "" {
		e.Actor, e.Rank = "luna", "luna"
	}
	if _, err := c.record(ctx, e); err != nil {
		return err
	}
	msg := "🛡️ Shield Mode is on"
	if reason != "" {
		msg += ": " + reason
	}
	return c.bot.Chat.Announce(ctx, msg+". Mods: !shield off to end it.")
}

func (c *core) setupModeration() {
	bot := c.bot
	bot.On("twitch.channel.follow", c.watchFollows)
	bot.On("twitch.channel.raid", c.raided)

	c.chat(luna.ChatCommand{Name: "permit", Capability: "mod.timeout"}, func(ctx *luna.Ctx, args []string) error {
		if len(args) != 1 {
			return ctx.Reply("Usage: !permit <user>")
		}
		who := login(args[0])
		if err := bot.Store.Set(ctx, "permit:"+who, true, time.Duration(c.cfg.ModPermitS)*time.Second); err != nil {
			return err
		}
		return ctx.Reply(fmt.Sprintf("%s may post one link in the next %ds.", who, c.cfg.ModPermitS))
	})
	c.chat(luna.ChatCommand{Name: "nuke", Capability: "mod.timeout"}, c.nuke)
	c.chat(luna.ChatCommand{Name: "mod", Capability: "mod.timeout"}, c.modCommand)
	c.chat(luna.ChatCommand{Name: "shield", Capability: "mod.shield"}, func(ctx *luna.Ctx, args []string) error {
		if len(args) != 1 || args[0] != "on" && args[0] != "off" {
			return ctx.Reply("Usage: !shield on|off")
		}
		return c.setShield(ctx, args[0] == "on", "")
	})
	c.chat(luna.ChatCommand{Name: "undo", Capability: "mod.undo.own"}, func(ctx *luna.Ctx, _ []string) error {
		es, err := c.timeline(ctx, 50)
		if err != nil {
			return err
		}
		for _, e := range es {
			if e.Undo != nil && !e.Undone {
				msg, err := c.undoEntry(ctx, e.ID, ctx.User().Login, ctx.User().Rank)
				if err != nil {
					return ctx.Reply("Can't: " + err.Error())
				}
				return ctx.Reply(msg)
			}
		}
		return ctx.Reply("Nothing to undo.")
	})

	bot.CLI(luna.CLICommand{Name: "timeline", Summary: "recent moderation and counter changes, newest first (also: luna mod timeline)",
		Flags: []luna.Arg{{Name: "n", Type: "int", Optional: true}}}, func(ctx *luna.Ctx, a luna.Args) (any, error) {
		n := 20
		if a.Has("n") {
			n = int(min(max(a.Int("n"), 1), timelineKeep))
		}
		es, err := c.timeline(ctx, n)
		rows := []map[string]any{}
		for _, e := range es {
			state := ""
			switch {
			case e.Undone:
				state = "undone"
			case e.Undo != nil:
				state = "undoable"
			}
			rows = append(rows, map[string]any{"id": e.ID, "at": e.At.Local().Format("Jan 2 15:04"), "by": e.Actor,
				"action": e.Action, "user": e.User, "detail": e.Detail, "state": state})
		}
		return rows, err
	})
	bot.CLI(luna.CLICommand{Name: "undo", Summary: "reverse a timeline entry", Args: []luna.Arg{{Name: "id", Type: "int"}}},
		func(ctx *luna.Ctx, a luna.Args) (any, error) {
			return c.undoEntry(ctx, a.Int("id"), "", "broadcaster") // the CLI is the streamer's own machine
		})
	bot.CLI(luna.CLICommand{Name: "phrases", Summary: "banned phrases (RE2, matched on a folded copy of each message)"},
		func(ctx *luna.Ctx, _ luna.Args) (any, error) {
			var pats []string
			_, err := bot.Store.Get(ctx, "phrases", &pats)
			if len(pats) == 0 {
				return "no banned phrases", err
			}
			return pats, err
		})
	bot.CLI(luna.CLICommand{Name: "phrase-add", Summary: "ban a phrase", Args: []luna.Arg{{Name: "pattern", Type: "str"}}, Mutates: true},
		func(ctx *luna.Ctx, a luna.Args) (any, error) { return c.editPhrases(ctx, a.Str("pattern"), true) })
	bot.CLI(luna.CLICommand{Name: "phrase-remove", Summary: "unban a phrase", Args: []luna.Arg{{Name: "pattern", Type: "str"}}, Mutates: true},
		func(ctx *luna.Ctx, a luna.Args) (any, error) { return c.editPhrases(ctx, a.Str("pattern"), false) })
}

func (c *core) editPhrases(ctx *luna.Ctx, pat string, add bool) (string, error) {
	if add {
		if len(pat) == 0 || len(pat) > 200 {
			return "", fmt.Errorf("a phrase is 1-200 characters")
		}
		if _, err := regexp.Compile("(?i)" + pat); err != nil { // RE2: linear time, so no ReDoS
			return "", fmt.Errorf("not a valid pattern: %v", err)
		}
	}
	var pats []string
	if _, err := c.bot.Store.Get(ctx, "phrases", &pats); err != nil {
		return "", err
	}
	had := slices.Contains(pats, pat)
	switch {
	case add && had:
		return "already banned", nil
	case add && len(pats) >= 200:
		return "", fmt.Errorf("200 phrases is the limit")
	case add:
		pats = append(pats, pat)
	case !had:
		return "", fmt.Errorf("%q isn't banned", pat)
	default:
		pats = slices.DeleteFunc(pats, func(p string) bool { return p == pat })
	}
	if err := c.bot.Store.Set(ctx, "phrases", pats, 0); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.phrasesRead = false
	c.mu.Unlock()
	if add {
		return "banned " + pat, nil
	}
	return "unbanned " + pat, nil
}

// nuke times out everyone who said phrase recently: !nuke <phrase> [60s], or !nuke undo.
func (c *core) nuke(ctx *luna.Ctx, args []string) error {
	if len(args) == 1 && args[0] == "undo" {
		es, err := c.timeline(ctx, 100)
		if err != nil {
			return err
		}
		for _, e := range es {
			if e.Action == "nuke" && !e.Undone {
				msg, err := c.undoEntry(ctx, e.ID, ctx.User().Login, ctx.User().Rank)
				if err != nil {
					return ctx.Reply("Can't: " + err.Error())
				}
				return ctx.Reply(msg)
			}
		}
		return ctx.Reply("No nuke to undo.")
	}
	look := 60 * time.Second
	if n := len(args); n > 1 {
		if d, err := time.ParseDuration(args[n-1]); err == nil && d > 0 && d <= 5*time.Minute {
			look, args = d, args[:n-1]
		}
	}
	phrase := fold(strings.Join(args, " "))
	if strings.TrimSpace(phrase) == "" {
		return ctx.Reply("Usage: !nuke <phrase> [60s] · !nuke undo")
	}
	now := c.bot.Now()
	var hit []string
	var logins []string
	c.mu.Lock()
	for _, m := range c.recent {
		if !m.mod && now.Sub(m.at) <= look && strings.Contains(m.folded, phrase) && !slices.Contains(hit, m.userID) {
			hit, logins = append(hit, m.userID), append(logins, m.login)
		}
	}
	c.mu.Unlock()
	d := time.Duration(c.cfg.ModNukeTimeoutS) * time.Second
	for _, id := range hit {
		if err := c.bot.Mod.Timeout(ctx, id, d, "nuke: "+phrase); err != nil {
			return err
		}
	}
	if len(hit) == 0 {
		return ctx.Reply("Nobody said that in the last " + human(look) + ".")
	}
	id, err := c.record(ctx, entry{Action: "nuke", Detail: fmt.Sprintf("%q: %s", phrase, strings.Join(logins, ", ")), Undo: &undo{Unban: hit}})
	if err != nil {
		return err
	}
	return ctx.Reply(fmt.Sprintf("Nuked %s (#%d; !nuke undo reverses it).", plural(int64(len(hit)), "chatter"), id))
}

// modCommand: !mod history <user> · !mod phrase add|remove <pattern>
func (c *core) modCommand(ctx *luna.Ctx, args []string) error {
	switch {
	case len(args) == 2 && args[0] == "history":
		return c.history(ctx, login(args[1]))
	case len(args) >= 3 && args[0] == "phrase" && (args[1] == "add" || args[1] == "remove"):
		msg, err := c.editPhrases(ctx, strings.Join(args[2:], " "), args[1] == "add")
		if err != nil {
			return ctx.Reply(err.Error())
		}
		return ctx.Reply(msg)
	}
	return ctx.Reply("Usage: !mod history <user> · !mod phrase add|remove <pattern>")
}

// history is one line about a viewer: first seen, strikes, timeouts and their latest note.
func (c *core) history(ctx *luna.Ctx, who string) error {
	parts := []string{who + ":"}
	v, err := c.bot.Viewers.Get(ctx, who)
	if err != nil {
		return err
	}
	if v == nil {
		parts = append(parts, "never chatted")
	} else {
		parts = append(parts, "first seen "+v.FirstSeen.Format("2006-01-02")+",", plural(v.Messages, "message")+",")
		var s strike
		if _, err := c.bot.Store.Get(ctx, "strike:"+v.ID, &s); err != nil {
			return err
		}
		parts = append(parts, plural(int64(s.N), "active strike")+",")
	}
	es, err := c.timeline(ctx, timelineKeep)
	if err != nil {
		return err
	}
	var timeouts int64
	for _, e := range es {
		if e.User == who && (e.Action == "timeout" || e.Action == "ban") || e.Action == "nuke" && strings.Contains(e.Detail, " "+who) {
			timeouts++
		}
	}
	parts = append(parts, plural(timeouts, "timeout"))
	var notes []note
	if _, err := c.bot.Store.Get(ctx, "notes:"+who, &notes); err != nil {
		return err
	}
	if len(notes) > 0 {
		n := notes[len(notes)-1]
		parts = append(parts, fmt.Sprintf("· note (%s, %s): %s", n.By, n.At.Format("Jan 2"), n.Text))
		if len(notes) > 1 {
			parts = append(parts, "(+"+strconv.Itoa(len(notes)-1)+" older)")
		}
	}
	return ctx.Reply(strings.Join(parts, " "))
}
