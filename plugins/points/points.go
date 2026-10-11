// Package points is luna.points: a loyalty currency earned by watching and chatting,
// with streaks and leaderboards. Other plugins spend it over links.
package points

import (
	_ "embed"
	"errors"
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

type Settings struct {
	Name           string `luna:"name"`
	Emoji          string `luna:"emoji"`
	WatchPoints    int    `luna:"watch_points"`
	WatchIntervalM int    `luna:"watch_interval_m"`
	ActiveWindowM  int    `luna:"active_window_m"`
	SubBonusPct    int    `luna:"sub_bonus_percent"`
	FollowBonus    int    `luna:"follow_bonus"`
	SubBonus       int    `luna:"sub_bonus"`
	GiftBonus      int    `luna:"gift_bonus"`
	BitsPer100     int    `luna:"bits_per_100"`
	RaidBonus      int    `luna:"raid_bonus"`
	StreakShout    bool   `luna:"streak_shout"`
	StreakMessage  string `luna:"streak_message"`
}

type chatter struct {
	last time.Time
	sub  bool
}

type points struct {
	bot *luna.Bot
	cfg Settings

	mu       sync.Mutex
	live     bool
	active   map[string]*chatter // user id → their latest message this stream
	streaked map[string]bool     // shouted this stream
	moveMu   sync.Mutex
}

// New builds the plugin's bot.
func New() *luna.Bot {
	bot := luna.New()
	p := &points{bot: bot, active: map[string]*chatter{}, streaked: map[string]bool{}}
	bot.UseConfig(&p.cfg)
	bot.OnStart(func(ctx *luna.Ctx) error {
		if s, err := bot.Twitch.Stream(ctx, ""); err == nil {
			p.mu.Lock()
			p.live = s.Live
			p.mu.Unlock()
		}
		bot.Timers.Every(time.Duration(max(p.cfg.WatchIntervalM, 1))*time.Minute, p.payWatchers)
		return nil
	})
	bot.On("twitch.stream.online", func(ctx *luna.Ctx, ev *luna.Event) error {
		p.mu.Lock()
		p.live, p.active, p.streaked = true, map[string]*chatter{}, map[string]bool{}
		p.mu.Unlock()
		return nil
	})
	bot.On("twitch.stream.offline", func(ctx *luna.Ctx, ev *luna.Event) error {
		p.mu.Lock()
		p.live = false
		p.mu.Unlock()
		return nil
	})
	bot.On("twitch.chat.message", p.chat)
	bonus := func(amount func(*luna.Event) int) func(*luna.Ctx, *luna.Event) error {
		return func(ctx *luna.Ctx, ev *luna.Event) error {
			u := ev.User()
			if n := amount(ev); u != nil && u.ID != "" && n > 0 && !ev.Bool("anonymous") {
				_, err := p.add(ctx, u.ID, int64(n))
				return err
			}
			return nil
		}
	}
	bot.On("twitch.channel.follow", bonus(func(*luna.Event) int { return p.cfg.FollowBonus }))
	bot.On("twitch.channel.subscribe", bonus(func(ev *luna.Event) int {
		if ev.Bool("gift") {
			return 0 // the gifter gets the bonus
		}
		return p.cfg.SubBonus
	}))
	bot.On("twitch.channel.resub", bonus(func(*luna.Event) int { return p.cfg.SubBonus }))
	bot.On("twitch.channel.gift", bonus(func(ev *luna.Event) int { return p.cfg.GiftBonus * int(ev.Int("count")) }))
	bot.On("twitch.channel.cheer", bonus(func(ev *luna.Event) int { return p.cfg.BitsPer100 * int(ev.Int("bits")) / 100 }))
	bot.On("twitch.channel.raid", bonus(func(*luna.Event) int { return p.cfg.RaidBonus }))

	bot.ChatCommand(luna.ChatCommand{Name: "points", UserCooldown: "5s"}, p.pointsCmd)
	bot.ChatCommand(luna.ChatCommand{Name: "top", Cooldown: "10s"}, func(ctx *luna.Ctx, _ []string) error {
		top, err := p.top(ctx, 5)
		if err != nil || len(top) == 0 {
			return err
		}
		var parts []string
		for i, r := range top {
			parts = append(parts, fmt.Sprintf("%d. %s %d", i+1, r.Login, r.Balance))
		}
		return ctx.Reply(fmt.Sprintf("Top %s: %s", p.cfg.Name, strings.Join(parts, " · ")))
	})
	bot.ChatCommand(luna.ChatCommand{Name: "give", UserCooldown: "5s"}, p.give)

	bot.CLI(luna.CLICommand{Name: "top", Summary: "the leaderboard", Flags: []luna.Arg{{Name: "n", Type: "int", Optional: true}}},
		func(ctx *luna.Ctx, a luna.Args) (any, error) {
			n := 10
			if a.Has("n") {
				n = int(a.Int("n"))
			}
			return p.top(ctx, n)
		})
	bot.CLI(luna.CLICommand{Name: "get", Summary: "someone's balance", Args: []luna.Arg{{Name: "user", Type: "str"}}},
		func(ctx *luna.Ctx, a luna.Args) (any, error) {
			id, name, err := p.resolve(ctx, a.Str("user"))
			if err != nil {
				return nil, err
			}
			n, err := p.balance(ctx, id)
			return fmt.Sprintf("%s has %s", name, p.amount(n)), err
		})
	bot.CLI(luna.CLICommand{Name: "add", Summary: "add (or with a negative amount, remove) points", Mutates: true,
		Args: []luna.Arg{{Name: "user", Type: "str"}, {Name: "amount", Type: "int"}}},
		func(ctx *luna.Ctx, a luna.Args) (any, error) {
			return p.adjust(ctx, "luna", a.Str("user"), a.Int("amount"))
		})
	bot.CLI(luna.CLICommand{Name: "ledger", Summary: "recent manual changes, gifts and spending, newest first",
		Flags: []luna.Arg{{Name: "n", Type: "int", Optional: true}}},
		func(ctx *luna.Ctx, a luna.Args) (any, error) {
			n := 20
			if a.Has("n") {
				n = int(a.Int("n"))
			}
			return p.ledger(ctx, n)
		})
	bot.CLI(luna.CLICommand{Name: "undo", Summary: "reverse a ledger entry", Mutates: true, Args: []luna.Arg{{Name: "id", Type: "int"}}},
		func(ctx *luna.Ctx, a luna.Args) (any, error) { return p.undo(ctx, a.Int("id")) })

	// Links: how Giveaways, Games and community plugins use the currency.
	bot.Links.Provide("balance", func(ctx *luna.Ctx, _ string, a luna.Args) (any, error) {
		n, err := p.balance(ctx, a.Str("user_id"))
		return map[string]any{"balance": n}, err
	})
	bot.Links.Provide("spend", func(ctx *luna.Ctx, caller string, a luna.Args) (any, error) {
		n := a.Int("amount")
		if n <= 0 {
			return nil, errors.New("amount must be positive")
		}
		bal, err := p.move(ctx, caller, a.Str("reason"), change{a.Str("user_id"), a.Str("login"), -n})
		if err != nil {
			return nil, err
		}
		return map[string]any{"balance": bal[0]}, nil
	})
	bot.Links.Provide("add", func(ctx *luna.Ctx, caller string, a luna.Args) (any, error) {
		n := a.Int("amount")
		if n <= 0 {
			return nil, errors.New("amount must be positive")
		}
		bal, err := p.move(ctx, caller, a.Str("reason"), change{a.Str("user_id"), a.Str("login"), n})
		if err != nil {
			return nil, err
		}
		return map[string]any{"balance": bal[0]}, nil
	})
	return bot
}

// Main runs the plugin as a capsule: `luna __points` inside the sandbox.
func Main() { New().Run() }

func (p *points) amount(n int64) string {
	s := strconv.FormatInt(n, 10) + " " + p.cfg.Name
	if p.cfg.Emoji != "" {
		s += " " + p.cfg.Emoji
	}
	return s
}

// shortcut: one store key per viewer, so the 10000-key plugin cap holds about 9000 balances; move balances to a host table if a channel outgrows it.
func balKey(id string) string { return "bal:" + id }

func (p *points) balance(ctx *luna.Ctx, id string) (int64, error) {
	if id == "" {
		return 0, errors.New("no user_id")
	}
	var n int64
	_, err := p.bot.Store.Get(ctx, balKey(id), &n)
	return n, err
}

// add credits id without a ledger entry: earnings are too many to list, and nobody undoes them.
func (p *points) add(ctx *luna.Ctx, id string, n int64) (int64, error) {
	return p.bot.Store.Incr(ctx, balKey(id), n)
}

func (p *points) chat(ctx *luna.Ctx, ev *luna.Event) error {
	u := ev.User()
	if u == nil || u.ID == "" {
		return nil
	}
	p.mu.Lock()
	if !p.live {
		p.mu.Unlock()
		return nil
	}
	p.active[u.ID] = &chatter{last: p.bot.Now(), sub: u.Has("sub")}
	first := !p.streaked[u.ID]
	p.streaked[u.ID] = true
	p.mu.Unlock()
	if !first || !p.cfg.StreakShout {
		return nil
	}
	v, err := p.bot.Viewers.Get(ctx, u.ID)
	if err != nil || v == nil || !milestone(v.Streak) {
		return err
	}
	return p.bot.Chat.Send(ctx, strings.NewReplacer("{user}", u.DisplayName(), "{streak}", strconv.FormatInt(v.Streak, 10)).Replace(p.cfg.StreakMessage))
}

func milestone(n int64) bool { return n == 5 || n == 10 || n == 25 || (n >= 50 && n%50 == 0) }

// payWatchers pays everyone who chatted within the active window, while live, so an idle
// tab left open earns nothing.
func (p *points) payWatchers(ctx *luna.Ctx) error {
	p.mu.Lock()
	if !p.live {
		p.mu.Unlock()
		return nil
	}
	cutoff := p.bot.Now().Add(-time.Duration(p.cfg.ActiveWindowM) * time.Minute)
	type pay struct {
		id string
		n  int64
	}
	var due []pay
	for id, c := range p.active {
		if c.last.Before(cutoff) {
			delete(p.active, id)
			continue
		}
		n := int64(p.cfg.WatchPoints)
		if c.sub {
			n += n * int64(p.cfg.SubBonusPct) / 100
		}
		due = append(due, pay{id, n})
	}
	p.mu.Unlock()
	// shortcut: one Incr per chatter, fine for hundreds of active chatters; batch it if a channel has thousands.
	for _, d := range due {
		if _, err := p.add(ctx, d.id, d.n); err != nil {
			return err
		}
	}
	return nil
}

type Row struct {
	Login   string `json:"login"`
	Balance int64  `json:"balance"`
}

// top reads every balance. shortcut: a full scan per call, fine to ~50k viewers; keep a sorted index past that.
func (p *points) top(ctx *luna.Ctx, n int) ([]Row, error) {
	keys, err := p.bot.Store.Keys(ctx, "bal:")
	if err != nil {
		return nil, err
	}
	var rows []Row
	for _, k := range keys {
		r := Row{Login: strings.TrimPrefix(k, "bal:")} // the id, until it's named below
		if _, err := p.bot.Store.Get(ctx, k, &r.Balance); err != nil {
			return nil, err
		}
		if r.Balance > 0 {
			rows = append(rows, r)
		}
	}
	slices.SortStableFunc(rows, func(a, b Row) int {
		if a.Balance != b.Balance {
			return int(b.Balance - a.Balance)
		}
		return strings.Compare(a.Login, b.Login)
	})
	rows = rows[:min(n, len(rows))]
	for i, r := range rows {
		if v, err := p.bot.Viewers.Get(ctx, r.Login); err == nil && v != nil {
			rows[i].Login = v.Login
		} else if tu, err := p.bot.Twitch.User(ctx, r.Login); err == nil && tu != nil {
			rows[i].Login = tu.Login
		}
	}
	return rows, nil
}

// resolve finds a viewer's id by login, from Luna's viewer profiles or Twitch.
func (p *points) resolve(ctx *luna.Ctx, name string) (string, string, error) {
	name = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "@"))
	if v, err := p.bot.Viewers.Get(ctx, name); err == nil && v != nil {
		return v.ID, v.Login, nil
	}
	tu, err := p.bot.Twitch.User(ctx, name)
	if err != nil {
		return "", "", err
	}
	if tu == nil {
		return "", "", fmt.Errorf("no Twitch user %q", name)
	}
	return tu.ID, tu.Login, nil
}

func (p *points) pointsCmd(ctx *luna.Ctx, args []string) error {
	u := ctx.User()
	if len(args) > 0 {
		switch verb := strings.ToLower(args[0]); verb {
		case "add", "remove", "undo":
			// shortcut: rank check mirrors points.adjust's default (Luna mod); read the streamer's capability table once the plugin API exposes it.
			if !u.RankAtLeast("luna-mod") {
				return nil
			}
			if verb == "undo" {
				id, err := strconv.ParseInt(strings.TrimPrefix(argAt(args, 1), "#"), 10, 64)
				if err != nil {
					return ctx.Reply("Usage: !points undo <id>")
				}
				msg, err := p.undo(ctx, id)
				if err != nil {
					return ctx.Reply(err.Error())
				}
				return ctx.Reply(msg)
			}
			n, err := strconv.ParseInt(argAt(args, 2), 10, 64)
			if err != nil || n <= 0 {
				return ctx.Reply(fmt.Sprintf("Usage: !points %s <user> <amount>", verb))
			}
			if verb == "remove" {
				n = -n
			}
			msg, err := p.adjust(ctx, u.Login, args[1], n)
			if err != nil {
				return ctx.Reply(err.Error())
			}
			return ctx.Reply(msg)
		}
		id, name, err := p.resolve(ctx, args[0])
		if err != nil {
			return ctx.Reply(err.Error())
		}
		n, err := p.balance(ctx, id)
		if err != nil {
			return err
		}
		return ctx.Reply(fmt.Sprintf("%s has %s.", name, p.amount(n)))
	}
	n, err := p.balance(ctx, u.ID)
	if err != nil {
		return err
	}
	return ctx.Reply(fmt.Sprintf("%s, you have %s.", u.DisplayName(), p.amount(n)))
}

func argAt(a []string, i int) string {
	if i < len(a) {
		return a[i]
	}
	return ""
}

func (p *points) give(ctx *luna.Ctx, args []string) error {
	u := ctx.User()
	n, err := strconv.ParseInt(argAt(args, 1), 10, 64)
	if len(args) != 2 || err != nil || n <= 0 {
		return ctx.Reply("Usage: !give <user> <amount>")
	}
	id, name, err := p.resolve(ctx, args[0])
	if err != nil {
		return ctx.Reply(err.Error())
	}
	if id == u.ID {
		return nil
	}
	if _, err := p.move(ctx, u.Login, "give", change{u.ID, u.Login, -n}, change{id, name, n}); err != nil {
		return ctx.Reply(err.Error())
	}
	return ctx.Reply(fmt.Sprintf("%s gave %s to %s.", u.DisplayName(), p.amount(n), name))
}

func (p *points) adjust(ctx *luna.Ctx, actor, user string, n int64) (string, error) {
	id, name, err := p.resolve(ctx, user)
	if err != nil {
		return "", err
	}
	bal, err := p.move(ctx, actor, "adjust", change{id, name, n})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s now has %s.", name, p.amount(bal[0])), nil
}

// ---------- the ledger: every change someone made on purpose, undoable ----------

type change struct {
	UserID string `json:"user_id"`
	Login  string `json:"login"`
	Delta  int64  `json:"delta"`
}

type Entry struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	Actor   string    `json:"actor"` // a login, or the plugin that spent the points
	Reason  string    `json:"reason,omitempty"`
	Changes []change  `json:"changes"`
	Undone  bool      `json:"undone,omitempty"`
}

const ledgerKeep = 1000

func ledKey(id int64) string { return fmt.Sprintf("led:%012d", id) }

var errNotEnough = errors.New("not enough points")

// move applies changes, refusing (and rolling back) if any balance would go negative,
// and records them in the ledger. It returns the new balances.
func (p *points) move(ctx *luna.Ctx, actor, reason string, cs ...change) ([]int64, error) {
	p.moveMu.Lock() // so the check-then-roll-back below can't interleave
	defer p.moveMu.Unlock()
	var out []int64
	for i, c := range cs {
		if c.UserID == "" {
			return nil, errors.New("no user_id")
		}
		bal, err := p.add(ctx, c.UserID, c.Delta)
		applied := i
		if err == nil && bal < 0 && c.Delta < 0 {
			err, applied = fmt.Errorf("%w: %s has %s", errNotEnough, cmp(c.Login, c.UserID), p.amount(bal-c.Delta)), i+1
		}
		if err != nil {
			for _, r := range cs[:applied] {
				p.bot.Store.Incr(ctx, balKey(r.UserID), -r.Delta)
			}
			return nil, err
		}
		out = append(out, bal)
	}
	id, err := p.bot.Store.Incr(ctx, "led.seq", 1)
	if err != nil {
		return out, err
	}
	var b luna.Batch
	b.Set(ledKey(id), Entry{ID: id, At: p.bot.Now(), Actor: actor, Reason: reason, Changes: cs}, 0)
	if id > ledgerKeep {
		b.Delete(ledKey(id - ledgerKeep))
	}
	return out, p.bot.Store.Apply(ctx, &b)
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (p *points) ledger(ctx *luna.Ctx, n int) ([]Entry, error) {
	keys, err := p.bot.Store.Keys(ctx, "led:")
	if err != nil {
		return nil, err
	}
	slices.Reverse(keys)
	out := []Entry{}
	for _, k := range keys[:min(n, len(keys))] {
		var e Entry
		if ok, err := p.bot.Store.Get(ctx, k, &e); err != nil {
			return nil, err
		} else if ok {
			out = append(out, e)
		}
	}
	return out, nil
}

func (p *points) undo(ctx *luna.Ctx, id int64) (string, error) {
	var e Entry
	if ok, err := p.bot.Store.Get(ctx, ledKey(id), &e); err != nil {
		return "", err
	} else if !ok {
		return "", fmt.Errorf("no ledger entry #%d", id)
	}
	if e.Undone {
		return "", fmt.Errorf("#%d is already undone", id)
	}
	p.moveMu.Lock()
	for _, c := range e.Changes {
		if _, err := p.bot.Store.Incr(ctx, balKey(c.UserID), -c.Delta); err != nil {
			p.moveMu.Unlock()
			return "", err
		}
	}
	p.moveMu.Unlock()
	e.Undone = true
	if err := p.bot.Store.Set(ctx, ledKey(id), e, 0); err != nil {
		return "", err
	}
	return fmt.Sprintf("Undid #%d (%s by %s).", id, cmp(e.Reason, "change"), e.Actor), nil
}
