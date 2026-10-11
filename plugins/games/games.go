// Package games is luna.games: duels, heists and trivia played with Points. Stakes are
// spent through the Points link when a game starts and paid out (or refunded) when it
// ends; each stake is recorded, so a restart refunds games it interrupted.
package games

import (
	_ "embed"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	luna "luna/sdk/go"
)

//go:embed plugin.toml
var Manifest []byte

// Question is one [[plugins.games.trivia]] entry.
type Question struct {
	Question string `json:"question"`
	Answer   string `json:"answer"` // accepted answers, separated by |
	Reward   int64  `json:"reward"`
}

type Settings struct {
	LiveOnly           bool       `luna:"live_only"`
	MaxBet             int        `luna:"max_bet"`
	DisabledCategories string     `luna:"disabled_categories"`
	DuelAcceptS        int        `luna:"duel_accept_s"`
	HeistJoinS         int        `luna:"heist_join_s"`
	HeistBaseChance    int        `luna:"heist_base_chance"`
	HeistPerMember     int        `luna:"heist_per_member"`
	HeistMaxChance     int        `luna:"heist_max_chance"`
	HeistPayoutPct     int        `luna:"heist_payout_percent"`
	Trivia             []Question `luna:"trivia"`
	TriviaAnswerS      int        `luna:"trivia_answer_s"`
}

const (
	spend = "luna.points:spend"
	add   = "luna.points:add"
)

// roll is a number in 0..n-1; tests make it predictable.
var roll = rand.IntN

type duel struct {
	from         *luna.User
	bet          int64
	stake        string
	cancelExpiry func()
}

type member struct {
	user  *luna.User
	bet   int64
	stake string
}

type games struct {
	bot *luna.Bot
	cfg Settings

	mu       sync.Mutex
	live     bool
	category string
	duels    map[string]*duel // challenged login → their pending duel
	heist    []member         // nil when no heist is forming
	question *Question        // the trivia question waiting for an answer
	endQ     func()
}

func New() *luna.Bot {
	bot, _ := build()
	return bot
}

func build() (*luna.Bot, *games) {
	bot := luna.New()
	g := &games{bot: bot, duels: map[string]*duel{}}
	bot.UseConfig(&g.cfg)
	bot.Links.Use(spend, add)
	bot.OnStart(func(ctx *luna.Ctx) error {
		if s, err := bot.Twitch.Stream(ctx, ""); err == nil {
			g.live = s.Live
		}
		if ch, err := bot.Twitch.Channel(ctx, ""); err == nil && ch != nil {
			g.category = ch.Game
		}
		return g.refundInterrupted(ctx)
	})
	bot.On("twitch.stream.online", func(*luna.Ctx, *luna.Event) error { g.set(func() { g.live = true }); return nil })
	bot.On("twitch.stream.offline", func(*luna.Ctx, *luna.Event) error { g.set(func() { g.live = false }); return nil })
	bot.On("twitch.channel.update", func(_ *luna.Ctx, ev *luna.Event) error {
		g.set(func() { g.category = ev.Str("game") })
		return nil
	})
	bot.On("twitch.chat.message", g.answer)
	bot.ChatCommand(luna.ChatCommand{Name: "duel", UserCooldown: "10s"}, g.duel)
	bot.ChatCommand(luna.ChatCommand{Name: "accept"}, g.accept)
	bot.ChatCommand(luna.ChatCommand{Name: "heist", UserCooldown: "10s"}, g.heistCmd)
	bot.ChatCommand(luna.ChatCommand{Name: "trivia", Cooldown: "60s"}, g.trivia)
	return bot, g
}

// Main runs the plugin as a capsule: `luna __games` inside the sandbox.
func Main() { New().Run() }

func (g *games) set(f func()) {
	g.mu.Lock()
	f()
	g.mu.Unlock()
}

// closed says why games can't be played right now, or "" if they can.
func (g *games) closed() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cfg.LiveOnly && !g.live {
		return "Games only run while the stream is live."
	}
	for _, c := range strings.Split(g.cfg.DisabledCategories, ",") {
		if c = strings.TrimSpace(c); c != "" && strings.EqualFold(c, g.category) {
			return "Games are off during " + g.category + "."
		}
	}
	return ""
}

// bet reads a stake from chat, within the configured maximum.
func (g *games) bet(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, errors.New("the bet must be a positive number of points")
	}
	if g.cfg.MaxBet > 0 && n > int64(g.cfg.MaxBet) {
		return 0, fmt.Errorf("the most you can bet is %d", g.cfg.MaxBet)
	}
	return n, nil
}

// ---------- stakes: points held by a game until it ends ----------

type stake struct {
	UserID string `json:"user_id"`
	Login  string `json:"login"`
	Amount int64  `json:"amount"`
	Game   string `json:"game"`
}

// hold spends u's bet and records it, so it can be paid out or refunded later.
func (g *games) hold(ctx *luna.Ctx, u *luna.User, amount int64, game string) (string, error) {
	err := g.bot.Links.Call(ctx, spend, map[string]any{"user_id": u.ID, "login": u.Login, "amount": amount, "reason": game}, nil)
	switch {
	case errors.Is(err, luna.ErrLinkUnavailable):
		return "", errors.New("Games need the Points plugin running.")
	case err != nil:
		return "", errors.New(strings.TrimPrefix(err.Error(), spend+": "))
	}
	n, err := g.bot.Store.Incr(ctx, "stake.seq", 1)
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("stake:%d", n)
	if err := g.bot.Store.Set(ctx, key, stake{u.ID, u.Login, amount, game}, 0); err != nil {
		g.bot.Links.Call(ctx, add, map[string]any{"user_id": u.ID, "amount": amount, "reason": game + " refund"}, nil)
		return "", err
	}
	return key, nil
}

// settle ends a stake, paying payout (0 when the bet was lost).
func (g *games) settle(ctx *luna.Ctx, key string, payout int64) error {
	var s stake
	if ok, err := g.bot.Store.Get(ctx, key, &s); err != nil || !ok {
		return err
	}
	if payout > 0 {
		if err := g.bot.Links.Call(ctx, add, map[string]any{"user_id": s.UserID, "login": s.Login, "amount": payout,
			"reason": s.Game}, nil); err != nil {
			return err // the stake stays recorded, so the next start refunds it
		}
	}
	return g.bot.Store.Delete(ctx, key)
}

// refundInterrupted returns the stakes of games a restart cut short.
func (g *games) refundInterrupted(ctx *luna.Ctx) error {
	keys, err := g.bot.Store.Keys(ctx, "stake:")
	if err != nil {
		return err
	}
	for _, k := range keys {
		var s stake
		if ok, err := g.bot.Store.Get(ctx, k, &s); err != nil || !ok {
			continue
		}
		if err := g.settle(ctx, k, s.Amount); err != nil {
			g.bot.Log.Warn(ctx, fmt.Sprintf("couldn't refund %s's %d from an interrupted %s yet: %v", s.Login, s.Amount, s.Game, err))
		}
	}
	return nil
}

// ---------- duel ----------

func (g *games) duel(ctx *luna.Ctx, args []string) error {
	if why := g.closed(); why != "" {
		return ctx.Reply(why)
	}
	if len(args) != 2 {
		return ctx.Reply("Usage: !duel <user> <points>")
	}
	u := ctx.User()
	target := strings.ToLower(strings.TrimPrefix(args[0], "@"))
	bet, err := g.bet(args[1])
	if err != nil {
		return ctx.Reply(err.Error())
	}
	if target == u.Login {
		return ctx.Reply("You can't duel yourself.")
	}
	g.mu.Lock()
	busy := g.duels[target] != nil
	for _, d := range g.duels {
		busy = busy || d.from.ID == u.ID
	}
	g.mu.Unlock()
	if busy {
		return ctx.Reply("One duel at a time: wait for the open one to finish.")
	}
	key, err := g.hold(ctx, u, bet, "duel")
	if err != nil {
		return ctx.Reply(err.Error())
	}
	d := &duel{from: u, bet: bet, stake: key}
	accept := time.Duration(g.cfg.DuelAcceptS) * time.Second
	g.mu.Lock()
	if g.duels[target] != nil { // challenged by someone else while the bet was being held
		g.mu.Unlock()
		if err := g.settle(ctx, key, bet); err != nil {
			return err
		}
		return ctx.Reply("One duel at a time: wait for the open one to finish.")
	}
	g.duels[target] = d
	d.cancelExpiry = g.bot.Timers.After(ctx, accept, func(ctx *luna.Ctx) error {
		g.mu.Lock()
		if g.duels[target] != d {
			g.mu.Unlock()
			return nil
		}
		delete(g.duels, target)
		g.mu.Unlock()
		if err := g.settle(ctx, d.stake, d.bet); err != nil {
			return err
		}
		return g.bot.Chat.Send(ctx, fmt.Sprintf("%s didn't accept the duel; %s gets their %d points back.", target, u.DisplayName(), bet))
	})
	g.mu.Unlock()
	return ctx.Reply(fmt.Sprintf("%s challenges %s to a duel for %d points! %s, type !accept within %s.", u.DisplayName(), target, bet,
		target, accept))
}

func (g *games) accept(ctx *luna.Ctx, _ []string) error {
	u := ctx.User()
	g.mu.Lock()
	d := g.duels[u.Login]
	delete(g.duels, u.Login)
	g.mu.Unlock()
	if d == nil {
		return nil
	}
	d.cancelExpiry()
	key, err := g.hold(ctx, u, d.bet, "duel")
	if err != nil {
		if serr := g.settle(ctx, d.stake, d.bet); serr != nil {
			return serr
		}
		return ctx.Reply(err.Error() + " The duel is off.")
	}
	winner, loser, wKey, lKey := d.from, u, d.stake, key
	if roll(2) == 1 {
		winner, loser, wKey, lKey = u, d.from, key, d.stake
	}
	if err := g.settle(ctx, wKey, 2*d.bet); err != nil {
		return err
	}
	if err := g.settle(ctx, lKey, 0); err != nil {
		return err
	}
	return ctx.Reply(fmt.Sprintf("⚔️ %s beats %s and wins %d points!", winner.DisplayName(), loser.DisplayName(), 2*d.bet))
}

// ---------- heist ----------

func (g *games) heistCmd(ctx *luna.Ctx, args []string) error {
	if why := g.closed(); why != "" {
		return ctx.Reply(why)
	}
	if len(args) != 1 {
		return ctx.Reply("Usage: !heist <points>")
	}
	u := ctx.User()
	bet, err := g.bet(args[0])
	if err != nil {
		return ctx.Reply(err.Error())
	}
	g.mu.Lock()
	joined := slices.ContainsFunc(g.heist, func(m member) bool { return m.user.ID == u.ID })
	g.mu.Unlock()
	if joined {
		return nil
	}
	key, err := g.hold(ctx, u, bet, "heist")
	if err != nil {
		return ctx.Reply(err.Error())
	}
	g.mu.Lock()
	first := g.heist == nil
	g.heist = append(g.heist, member{u, bet, key})
	g.mu.Unlock()
	if !first {
		return nil // joining quietly: a reply per member would flood chat
	}
	wait := time.Duration(g.cfg.HeistJoinS) * time.Second
	g.bot.Timers.After(ctx, wait, g.runHeist)
	return ctx.Reply(fmt.Sprintf("💰 %s is planning a heist! Join with !heist <points> in the next %s.", u.DisplayName(), wait))
}

func (g *games) runHeist(ctx *luna.Ctx) error {
	g.mu.Lock()
	crew := g.heist
	g.heist = nil
	g.mu.Unlock()
	chance := min(g.cfg.HeistBaseChance+g.cfg.HeistPerMember*(len(crew)-1), g.cfg.HeistMaxChance)
	won := roll(100) < chance
	var names []string
	for _, m := range crew {
		payout := int64(0)
		if won {
			payout = m.bet * int64(g.cfg.HeistPayoutPct) / 100
		}
		if err := g.settle(ctx, m.stake, payout); err != nil {
			return err
		}
		names = append(names, m.user.DisplayName())
	}
	if won {
		return g.bot.Chat.Send(ctx, fmt.Sprintf("🎉 The heist worked (%d%% chance)! %s each get %d%% of their bet back.",
			chance, strings.Join(names, ", "), g.cfg.HeistPayoutPct))
	}
	return g.bot.Chat.Send(ctx, fmt.Sprintf("🚨 The heist failed (%d%% chance). %s lost their bets.", chance, strings.Join(names, ", ")))
}

// ---------- trivia ----------

func (g *games) trivia(ctx *luna.Ctx, _ []string) error {
	if why := g.closed(); why != "" {
		return ctx.Reply(why)
	}
	if len(g.cfg.Trivia) == 0 {
		return ctx.Reply("No trivia questions yet: add [[plugins.games.trivia]] entries to luna.toml.")
	}
	g.mu.Lock()
	if g.question != nil {
		g.mu.Unlock()
		return nil
	}
	q := g.cfg.Trivia[roll(len(g.cfg.Trivia))]
	g.question = &q
	g.endQ = g.bot.Timers.After(ctx, time.Duration(g.cfg.TriviaAnswerS)*time.Second, func(ctx *luna.Ctx) error {
		g.mu.Lock()
		if g.question != &q {
			g.mu.Unlock()
			return nil
		}
		g.question = nil
		g.mu.Unlock()
		return g.bot.Chat.Send(ctx, "⏰ Time's up! The answer was: "+strings.Split(q.Answer, "|")[0])
	})
	g.mu.Unlock()
	return ctx.Reply(fmt.Sprintf("❓ %s (first right answer wins %d points)", q.Question, q.Reward))
}

func (g *games) answer(ctx *luna.Ctx, ev *luna.Event) error {
	u := ev.User()
	text := strings.ToLower(strings.TrimSpace(ev.Str("text")))
	g.mu.Lock()
	q := g.question
	right := q != nil && u != nil && slices.ContainsFunc(strings.Split(q.Answer, "|"), func(a string) bool {
		return strings.ToLower(strings.TrimSpace(a)) == text
	})
	if right {
		g.question = nil
		g.endQ()
	}
	g.mu.Unlock()
	if !right {
		return nil
	}
	if q.Reward > 0 {
		if err := g.bot.Links.Call(ctx, add, map[string]any{"user_id": u.ID, "login": u.Login, "amount": q.Reward,
			"reason": "trivia"}, nil); err != nil {
			return err
		}
	}
	return ctx.Reply(fmt.Sprintf("✅ %s got it: %s! +%d points", u.DisplayName(), strings.Split(q.Answer, "|")[0], q.Reward))
}
