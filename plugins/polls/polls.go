// Package polls is luna.polls: Twitch's native polls and predictions from chat, and chat
// polls (!vote) for channels that don't have native ones. Every change to a running poll
// is emitted as plugin.luna.polls.update, for overlays.
package polls

import (
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	luna "luna/sdk/go"
)

//go:embed plugin.toml
var Manifest []byte

type Settings struct {
	Native          bool `luna:"native"`
	DefaultDuration int  `luna:"default_duration_s"`
}

type option struct {
	Title string `json:"title"`
	Votes int    `json:"votes"`
}

// chatPoll is a poll counted in chat. shortcut: kept in memory, so a restart ends it; persist if streamers run long polls.
type chatPoll struct {
	Question string         `json:"question"`
	Options  []option       `json:"options"`
	votes    map[string]int // user id → option index
	end      func()
}

type polls struct {
	bot *luna.Bot
	cfg Settings

	mu         sync.Mutex
	chat       *chatPoll
	nativePoll string // the running native poll's id
	predict    *luna.Prediction
}

func New() *luna.Bot {
	bot := luna.New()
	p := &polls{bot: bot}
	bot.UseConfig(&p.cfg)
	bot.ChatCommand(luna.ChatCommand{Name: "poll", Capability: "polls.run"}, p.poll)
	bot.ChatCommand(luna.ChatCommand{Name: "vote"}, p.vote)
	bot.ChatCommand(luna.ChatCommand{Name: "predict", Capability: "polls.run"}, p.prediction)
	return bot
}

// Main runs the plugin as a capsule: `luna __polls` inside the sandbox.
func Main() { New().Run() }

// parse reads "question | a | b [duration=2m]".
func (p *polls) parse(args []string) (string, []string, time.Duration, error) {
	d := time.Duration(p.cfg.DefaultDuration) * time.Second
	var words []string
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "duration="); ok {
			var err error
			if d, err = time.ParseDuration(v); err != nil {
				return "", nil, 0, fmt.Errorf("duration=%s: %v", v, err)
			}
			continue
		}
		words = append(words, a)
	}
	var parts []string
	for _, s := range strings.Split(strings.Join(words, " "), "|") {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) < 3 {
		return "", nil, 0, errors.New("give a question and at least two options, split by |")
	}
	return parts[0], parts[1:], d, nil
}

func tooLong(question string, qMax int, options []string) error {
	if utf8.RuneCountInString(question) > qMax {
		return fmt.Errorf("Twitch allows questions up to %d characters.", qMax)
	}
	for _, o := range options {
		if utf8.RuneCountInString(o) > 25 {
			return fmt.Errorf("%q is over Twitch's 25 characters for an option.", o)
		}
	}
	return nil
}

// ---------- polls ----------

func (p *polls) poll(ctx *luna.Ctx, args []string) error {
	if len(args) == 1 && strings.EqualFold(args[0], "end") {
		return p.endPoll(ctx)
	}
	if len(args) == 0 {
		return ctx.Reply("Usage: !poll <question> | <option> | <option> [duration=2m] · !poll end")
	}
	q, opts, d, err := p.parse(args)
	switch {
	case err != nil:
		return ctx.Reply(err.Error())
	case len(opts) > 5:
		return ctx.Reply("A poll has 2 to 5 options.")
	case d < 15*time.Second || d > 30*time.Minute:
		return ctx.Reply("A poll runs 15 seconds to 30 minutes.")
	}
	if err := tooLong(q, 60, opts); err != nil {
		return ctx.Reply(err.Error())
	}
	p.mu.Lock()
	busy := p.chat != nil || p.nativePoll != ""
	p.mu.Unlock()
	if busy {
		return ctx.Reply("A poll is already running: !poll end first.")
	}
	note := ""
	if p.cfg.Native {
		np, err := p.bot.Twitch.StartPoll(ctx, q, opts, d)
		if err == nil {
			p.mu.Lock()
			p.nativePoll = np.ID
			p.mu.Unlock()
			p.bot.Timers.After(ctx, d, func(*luna.Ctx) error {
				p.mu.Lock()
				if p.nativePoll == np.ID {
					p.nativePoll = "" // Twitch posts the results itself
				}
				p.mu.Unlock()
				return nil
			})
			return nil // Twitch shows the poll in chat
		}
		note = " (Twitch polls aren't available: " + reason(err) + ")"
	}
	cp := &chatPoll{Question: q, votes: map[string]int{}}
	for _, o := range opts {
		cp.Options = append(cp.Options, option{Title: o})
	}
	p.mu.Lock()
	p.chat = cp
	cp.end = p.bot.Timers.After(ctx, d, func(ctx *luna.Ctx) error { return p.finishChat(ctx, cp) })
	p.mu.Unlock()
	p.emit(ctx, cp, true)
	var list []string
	for i, o := range opts {
		list = append(list, fmt.Sprintf("%d) %s", i+1, o))
	}
	return ctx.Reply(fmt.Sprintf("📊 %s %s. Vote with !vote <number> for %s.%s", q, strings.Join(list, " "), d, note))
}

func (p *polls) vote(ctx *luna.Ctx, args []string) error {
	p.mu.Lock()
	cp := p.chat
	if cp == nil || len(args) == 0 {
		p.mu.Unlock()
		return nil
	}
	n, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
	if err != nil || n < 1 || n > len(cp.Options) {
		p.mu.Unlock()
		return nil
	}
	uid := ctx.User().ID
	if prev, ok := cp.votes[uid]; ok {
		cp.Options[prev].Votes-- // changing your mind moves your vote
	}
	cp.votes[uid] = n - 1
	cp.Options[n-1].Votes++
	p.mu.Unlock()
	p.emit(ctx, cp, true)
	return nil // no reply per vote: it would flood chat
}

func (p *polls) endPoll(ctx *luna.Ctx) error {
	p.mu.Lock()
	cp, id := p.chat, p.nativePoll
	p.nativePoll = ""
	p.mu.Unlock()
	switch {
	case cp != nil:
		cp.end()
		return p.finishChat(ctx, cp)
	case id != "":
		np, err := p.bot.Twitch.EndPoll(ctx, id)
		if err != nil {
			return ctx.Reply("Couldn't end the poll: " + reason(err))
		}
		var opts []option
		for _, c := range np.Choices {
			opts = append(opts, option{c.Title, c.Votes})
		}
		return ctx.Reply(results(np.Title, opts))
	}
	return ctx.Reply("No poll is running.")
}

func (p *polls) finishChat(ctx *luna.Ctx, cp *chatPoll) error {
	p.mu.Lock()
	if p.chat != cp {
		p.mu.Unlock()
		return nil
	}
	p.chat = nil
	p.mu.Unlock()
	p.emit(ctx, cp, false)
	return p.bot.Chat.Send(ctx, results(cp.Question, cp.Options))
}

func results(q string, opts []option) string {
	total := 0
	for _, o := range opts {
		total += o.Votes
	}
	if total == 0 {
		return fmt.Sprintf("📊 %s: no votes.", q)
	}
	var parts []string
	best := 0
	for i, o := range opts {
		parts = append(parts, fmt.Sprintf("%s %d (%d%%)", o.Title, o.Votes, (o.Votes*100+total/2)/total))
		if o.Votes > opts[best].Votes {
			best = i
		}
	}
	return fmt.Sprintf("📊 %s: %s. Winner: %s", q, strings.Join(parts, " · "), opts[best].Title)
}

// emit feeds overlays: plugin.luna.polls.update with the question, options and votes.
func (p *polls) emit(ctx *luna.Ctx, cp *chatPoll, open bool) {
	p.mu.Lock()
	data := map[string]any{"question": cp.Question, "options": append([]option(nil), cp.Options...), "open": open}
	p.mu.Unlock()
	if err := p.bot.Emit(ctx, "update", data); err != nil {
		p.bot.Log.Warn(ctx, "poll update not published: "+err.Error())
	}
}

// ---------- predictions (native only: viewers bet channel points) ----------

func (p *polls) prediction(ctx *luna.Ctx, args []string) error {
	verb := ""
	if len(args) > 0 {
		verb = strings.ToLower(args[0])
	}
	p.mu.Lock()
	pr := p.predict
	p.mu.Unlock()
	switch verb {
	case "lock", "cancel", "win":
		if pr == nil {
			return ctx.Reply("No prediction is running.")
		}
		status, winner := map[string]string{"lock": "LOCKED", "cancel": "CANCELED", "win": "RESOLVED"}[verb], ""
		if verb == "win" {
			n, err := strconv.Atoi(argAt(args, 1))
			if err != nil || n < 1 || n > len(pr.Outcomes) {
				return ctx.Reply(fmt.Sprintf("Usage: !predict win <1-%d>", len(pr.Outcomes)))
			}
			winner = pr.Outcomes[n-1].ID
		}
		if _, err := p.bot.Twitch.EndPrediction(ctx, pr.ID, status, winner); err != nil {
			return ctx.Reply("Twitch refused: " + reason(err))
		}
		if verb != "lock" {
			p.mu.Lock()
			p.predict = nil
			p.mu.Unlock()
		}
		return nil // Twitch announces it in chat
	case "":
		return ctx.Reply("Usage: !predict <question> | <outcome> | <outcome> [duration=2m] · lock · win <n> · cancel")
	}
	if pr != nil {
		return ctx.Reply("A prediction is already running: !predict win <n> or !predict cancel first.")
	}
	q, outs, d, err := p.parse(args)
	switch {
	case err != nil:
		return ctx.Reply(err.Error())
	case len(outs) > 10:
		return ctx.Reply("A prediction has 2 to 10 outcomes.")
	case d < 30*time.Second || d > 30*time.Minute:
		return ctx.Reply("Predictions stay open 30 seconds to 30 minutes.")
	}
	if err := tooLong(q, 45, outs); err != nil {
		return ctx.Reply(err.Error())
	}
	np, err := p.bot.Twitch.StartPrediction(ctx, q, outs, d)
	if err != nil {
		return ctx.Reply("Twitch predictions aren't available: " + reason(err))
	}
	p.mu.Lock()
	p.predict = np
	p.mu.Unlock()
	return nil
}

// reason is an error as chat should read it, without the SDK's "luna: " prefix.
func reason(err error) string { return strings.TrimPrefix(err.Error(), "luna: ") }

func argAt(a []string, i int) string {
	if i < len(a) {
		return a[i]
	}
	return ""
}
