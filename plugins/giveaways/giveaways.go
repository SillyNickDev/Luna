// Package giveaways is luna.giveaways: keyword and ticket giveaways whose draws anyone can
// check afterwards. Luna commits to a random seed when the giveaway starts, and reveals it
// at the draw, so the winner follows from the seed and the entry list alone.
package giveaways

import (
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
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
	LiveOnly       bool   `luna:"live_only"`
	Keyword        string `luna:"keyword"`
	ClaimS         int    `luna:"claim_s"`
	ExcludeMods    bool   `luna:"exclude_mods"`
	MinAccountDays int    `luna:"min_account_days"`
	WinnerCooldown int    `luna:"winner_cooldown_days"`
	SubLuck        int    `luna:"sub_luck"`
	VIPLuck        int    `luna:"vip_luck"`
	LuckCap        int    `luna:"luck_cap"`
	StartMessage   string `luna:"start_message"`
	WinnerMessage  string `luna:"winner_message"`
}

// Giveaway is the running (or last) giveaway. Entries are stored one key each.
type Giveaway struct {
	ID         int64     `json:"id"`
	Prize      string    `json:"prize"`
	Keyword    string    `json:"keyword"`
	Started    time.Time `json:"started"`
	Ends       time.Time `json:"ends,omitzero"`
	Closed     time.Time `json:"closed,omitzero"` // when entries stopped: close, the timer or the first draw
	Open       bool      `json:"open"`
	Who        string    `json:"who"` // everyone, followers, subs
	MinFollowD int       `json:"min_follow_days,omitempty"`
	TicketCost int64     `json:"ticket_cost,omitempty"` // 0: keyword mode
	MaxTickets int       `json:"max_tickets,omitempty"`
	Commit     string    `json:"commit"`         // sha256 of the seed, posted at start
	Seed       string    `json:"seed,omitempty"` // revealed at the first draw
	Draws      []Draw    `json:"draws,omitempty"`
	Cancelled  bool      `json:"cancelled,omitempty"`
}

type Entrant struct {
	UserID  string `json:"user_id"`
	Login   string `json:"login"`
	Tickets int    `json:"tickets"`
	Spent   int64  `json:"spent,omitempty"`
}

type Draw struct {
	N       int       `json:"n"`
	UserID  string    `json:"user_id"`
	Login   string    `json:"login"`
	At      time.Time `json:"at"`
	Claimed bool      `json:"claimed,omitempty"`
}

type giveaways struct {
	bot *luna.Bot
	cfg Settings

	mu       sync.Mutex
	live     bool
	cur      *Giveaway       // nil before the first one
	rejected map[string]bool // told once why they can't enter
	closeT   func()
	claimT   func()
}

const (
	pointsSpend = "luna.points:spend"
	pointsAdd   = "luna.points:add"
)

func New() *luna.Bot {
	bot := luna.New()
	g := &giveaways{bot: bot, rejected: map[string]bool{}}
	bot.UseConfig(&g.cfg)
	bot.Links.Use(pointsSpend, pointsAdd)
	bot.OnStart(func(ctx *luna.Ctx) error {
		if s, err := bot.Twitch.Stream(ctx, ""); err == nil {
			g.live = s.Live
		}
		var cur Giveaway
		if ok, err := bot.Store.Get(ctx, "cur", &cur); err != nil {
			return err
		} else if ok {
			g.mu.Lock()
			g.cur = &cur
			g.mu.Unlock()
			if cur.Open && !cur.Ends.IsZero() {
				g.closeT = bot.Timers.After(ctx, max(cur.Ends.Sub(bot.Now()), 0), g.autoClose)
			}
		}
		return nil
	})
	bot.On("twitch.stream.online", func(ctx *luna.Ctx, ev *luna.Event) error { g.setLive(true); return nil })
	bot.On("twitch.stream.offline", func(ctx *luna.Ctx, ev *luna.Event) error { g.setLive(false); return nil })
	bot.On("twitch.chat.message", g.chat)
	bot.ChatCommand(luna.ChatCommand{Name: "giveaway", Aliases: []string{"ga"}, Capability: "giveaway.run"}, g.command)

	bot.CLI(luna.CLICommand{Name: "status", Summary: "the current giveaway and its entries"},
		func(ctx *luna.Ctx, _ luna.Args) (any, error) { return g.export(ctx) })
	bot.CLI(luna.CLICommand{Name: "export", Summary: "everything needed to re-run the draws: seed, entries, draws (JSON)"},
		func(ctx *luna.Ctx, _ luna.Args) (any, error) { return g.export(ctx) })
	bot.CLI(luna.CLICommand{Name: "verify", Summary: "re-run every draw from the seed and entry list, and check the winners match"},
		func(ctx *luna.Ctx, _ luna.Args) (any, error) {
			e, err := g.export(ctx)
			if err != nil {
				return nil, err
			}
			return Verify(e)
		})
	bot.CLI(luna.CLICommand{Name: "winners", Summary: "past winners, newest first, with their viewer history for delivery",
		Flags: []luna.Arg{{Name: "user", Type: "str", Optional: true}}},
		func(ctx *luna.Ctx, a luna.Args) (any, error) { return g.winners(ctx, a.Str("user")) })
	return bot
}

// Main runs the plugin as a capsule: `luna __giveaways` inside the sandbox.
func Main() { New().Run() }

func (g *giveaways) setLive(v bool) {
	g.mu.Lock()
	g.live = v
	g.mu.Unlock()
}

func (g *giveaways) save(ctx *luna.Ctx, ga *Giveaway) error {
	return g.bot.Store.Set(ctx, "cur", ga, 0)
}

// accepts reports whether an entry sent at ts counts. Entries are judged by when they were
// sent, so a chat backlog (replies wait on the rate limit) can't push them past the deadline.
func (ga *Giveaway) accepts(ts time.Time) bool {
	switch {
	case ga.Cancelled || len(ga.Draws) > 0:
		return false // the list is frozen once anyone has been drawn
	case !ga.Closed.IsZero():
		return ts.Before(ga.Closed)
	}
	return ga.Open && (ga.Ends.IsZero() || ts.Before(ga.Ends))
}

func entryKey(gid int64, uid string) string { return fmt.Sprintf("e:%d:%s", gid, uid) }
func winKey(gid int64, n int) string        { return fmt.Sprintf("win:%012d:%03d", gid, n) }

// ---------- chat: !giveaway <verb> ----------

func (g *giveaways) command(ctx *luna.Ctx, args []string) error {
	verb := ""
	if len(args) > 0 {
		verb = strings.ToLower(args[0])
	}
	var msg string
	var err error
	switch verb {
	case "start":
		msg, err = g.start(ctx, args[1:])
	case "close":
		msg, err = g.close(ctx)
	case "draw", "reroll":
		msg, err = g.draw(ctx, verb == "reroll")
	case "cancel":
		msg, err = g.cancel(ctx)
	case "verify":
		msg, err = g.verifyMsg(ctx)
	default:
		msg = "Usage: !giveaway start <prize> [keyword=!enter duration=5m who=followers|subs min_follow=7d tickets=<cost> max=<n>] | close | draw | reroll | cancel | verify"
	}
	if err != nil {
		msg = err.Error()
	}
	if msg == "" {
		return nil
	}
	return ctx.Reply(msg)
}

func (g *giveaways) start(ctx *luna.Ctx, args []string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cur != nil && g.cur.Open {
		return "", errors.New("A giveaway is already running: !giveaway close or !giveaway cancel it first.")
	}
	if g.cfg.LiveOnly && !g.live {
		return "", errors.New("Giveaways only run while you're live (set plugins.giveaways.live_only = false to change that).")
	}
	ga := &Giveaway{Keyword: g.cfg.Keyword, Who: "everyone", Started: g.bot.Now(), Open: true}
	var prize []string
	var dur time.Duration
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			prize = append(prize, a)
			continue
		}
		var err error
		switch strings.ToLower(k) {
		case "keyword":
			ga.Keyword = strings.ToLower(v)
		case "duration":
			dur, err = time.ParseDuration(v)
		case "who":
			ga.Who = strings.ToLower(v)
			if !slices.Contains([]string{"everyone", "followers", "subs"}, ga.Who) {
				err = errors.New("who= is everyone, followers or subs")
			}
		case "min_follow":
			ga.MinFollowD, err = days(v)
			if ga.Who == "everyone" {
				ga.Who = "followers"
			}
		case "tickets":
			ga.TicketCost, err = strconv.ParseInt(v, 10, 64)
			if err == nil && ga.TicketCost <= 0 {
				err = errors.New("tickets= is the points cost of one ticket")
			}
		case "max":
			ga.MaxTickets, err = strconv.Atoi(v)
		default:
			err = fmt.Errorf("unknown option %s=", k)
		}
		if err != nil {
			return "", fmt.Errorf("%s: %v", a, err)
		}
	}
	ga.Prize = strings.Trim(strings.Join(prize, " "), `"'`)
	if ga.Prize == "" || ga.Keyword == "" {
		return "", errors.New("Usage: !giveaway start <prize> [keyword=!enter duration=5m ...]")
	}
	if ga.TicketCost > 0 && ga.MaxTickets <= 0 {
		ga.MaxTickets = 10
	}
	seed := make([]byte, 32)
	rand.Read(seed)
	sum := sha256.Sum256(seed)
	ga.Seed, ga.Commit = hex.EncodeToString(seed), hex.EncodeToString(sum[:])
	id, err := g.bot.Store.Incr(ctx, "seq", 1)
	if err != nil {
		return "", err
	}
	ga.ID = id
	if dur > 0 {
		ga.Ends = ga.Started.Add(dur)
	}
	if err := g.clearEntries(ctx, id-1); err != nil {
		return "", err
	}
	if err := g.save(ctx, ga); err != nil {
		return "", err
	}
	g.cur, g.rejected = ga, map[string]bool{}
	if dur > 0 {
		g.closeT = g.bot.Timers.After(ctx, dur, g.autoClose)
	}
	how := "type " + ga.Keyword + " to enter"
	if ga.TicketCost > 0 {
		how = fmt.Sprintf("type %s <n> to buy tickets at %d points each, up to %d", ga.Keyword, ga.TicketCost, ga.MaxTickets)
	}
	if ga.Who != "everyone" {
		how += " (" + ga.Who + " only)"
	}
	if dur > 0 {
		how += ", for " + dur.String()
	}
	return render(g.cfg.StartMessage, map[string]string{"prize": ga.Prize, "how": how, "commit": ga.Commit[:16]}), nil
}

func days(v string) (int, error) {
	if s, ok := strings.CutSuffix(v, "d"); ok {
		return strconv.Atoi(s)
	}
	d, err := time.ParseDuration(v)
	return int(d.Hours() / 24), err
}

// clearEntries drops an older giveaway's entries; its summary stays in "cur" until replaced.
func (g *giveaways) clearEntries(ctx *luna.Ctx, gid int64) error {
	keys, err := g.bot.Store.Keys(ctx, fmt.Sprintf("e:%d:", gid))
	if err != nil || len(keys) == 0 {
		return err
	}
	var b luna.Batch
	for _, k := range keys {
		b.Delete(k)
	}
	return g.bot.Store.Apply(ctx, &b)
}

func (g *giveaways) autoClose(ctx *luna.Ctx) error {
	msg, err := g.close(ctx)
	if err != nil || msg == "" {
		return err
	}
	return g.bot.Chat.Send(ctx, msg)
}

func (g *giveaways) close(ctx *luna.Ctx) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cur == nil || !g.cur.Open {
		return "", nil
	}
	g.cur.Open, g.cur.Closed = false, g.bot.Now()
	if !g.cur.Ends.IsZero() && g.cur.Ends.Before(g.cur.Closed) {
		g.cur.Closed = g.cur.Ends // the timer fired a moment late
	}
	if g.closeT != nil {
		g.closeT()
		g.closeT = nil
	}
	if err := g.save(ctx, g.cur); err != nil {
		return "", err
	}
	es, err := g.entries(ctx, g.cur.ID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Entries for %s are closed: %d entered. A mod can !giveaway draw now.", g.cur.Prize, len(es)), nil
}

func (g *giveaways) cancel(ctx *luna.Ctx) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cur == nil || g.cur.Cancelled || len(g.cur.Draws) > 0 {
		return "", errors.New("There's no giveaway to cancel.")
	}
	es, err := g.entries(ctx, g.cur.ID)
	if err != nil {
		return "", err
	}
	refunded := 0
	for _, e := range es {
		if e.Spent > 0 {
			if err := g.bot.Links.Call(ctx, pointsAdd, map[string]any{"user_id": e.UserID, "login": e.Login, "amount": e.Spent,
				"reason": "giveaway refund"}, nil); err != nil {
				return "", fmt.Errorf("refunds stopped after %d (run cancel again to finish): %v", refunded, err)
			}
			e.Spent = 0 // refunded: a second cancel after a failure mustn't pay it again
			if err := g.bot.Store.Set(ctx, entryKey(g.cur.ID, e.UserID), e, 0); err != nil {
				return "", err
			}
			refunded++
		}
	}
	g.cur.Open, g.cur.Cancelled = false, true
	if g.closeT != nil {
		g.closeT()
		g.closeT = nil
	}
	if err := g.save(ctx, g.cur); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("The %s giveaway was cancelled.", g.cur.Prize)
	if refunded > 0 {
		msg += fmt.Sprintf(" Tickets refunded to %d people.", refunded)
	}
	return msg, nil
}

// ---------- entering ----------

func (g *giveaways) chat(ctx *luna.Ctx, ev *luna.Event) error {
	u := ev.User()
	text := strings.Fields(strings.ToLower(ev.Str("text")))
	if u == nil || len(text) == 0 {
		return nil
	}
	g.mu.Lock()
	ga := g.cur
	if ga != nil && !ga.Open && len(ga.Draws) > 0 {
		// The latest winner claims by chatting.
		d := &ga.Draws[len(ga.Draws)-1]
		if d.UserID == u.ID && !d.Claimed && g.claimT != nil {
			d.Claimed = true
			g.claimT()
			g.claimT = nil
			err := g.save(ctx, ga)
			if err == nil {
				err = g.bot.Store.Set(ctx, winKey(ga.ID, d.N), Win{Draw: *d, Prize: ga.Prize, Giveaway: ga.ID}, 0)
			}
			g.mu.Unlock()
			if err == nil {
				err = g.bot.Chat.Send(ctx, fmt.Sprintf("@%s claimed %s! 🎉", u.DisplayName(), ga.Prize))
			}
			return err
		}
	}
	if ga == nil || text[0] != ga.Keyword || !ga.accepts(ev.TS) {
		g.mu.Unlock()
		return nil
	}
	defer g.mu.Unlock()
	why, err := g.eligible(ctx, ga, u)
	if err != nil {
		return err
	}
	if why != "" {
		if g.rejected[u.ID] {
			return nil
		}
		g.rejected[u.ID] = true
		return ctx.Reply(why)
	}
	var e Entrant
	key := entryKey(ga.ID, u.ID)
	had, err := g.bot.Store.Get(ctx, key, &e)
	if err != nil {
		return err
	}
	e.UserID, e.Login = u.ID, u.Login
	if ga.TicketCost == 0 {
		if had {
			return nil
		}
		e.Tickets = g.luck(u)
		return g.bot.Store.Set(ctx, key, e, 0)
	}
	want := 1
	if len(text) > 1 {
		if want, err = strconv.Atoi(text[1]); err != nil || want <= 0 {
			return ctx.Reply(fmt.Sprintf("Usage: %s <tickets>", ga.Keyword))
		}
	}
	want = min(want, ga.MaxTickets-e.Tickets)
	if want <= 0 {
		return ctx.Reply(fmt.Sprintf("You already have the maximum of %d tickets.", ga.MaxTickets))
	}
	cost := int64(want) * ga.TicketCost
	err = g.bot.Links.Call(ctx, pointsSpend, map[string]any{"user_id": u.ID, "login": u.Login, "amount": cost,
		"reason": "giveaway tickets"}, nil)
	switch {
	case errors.Is(err, luna.ErrLinkUnavailable):
		return ctx.Reply("Ticket giveaways need the Points plugin running: luna capsule install points")
	case err != nil:
		return ctx.Reply(strings.TrimPrefix(err.Error(), pointsSpend+": "))
	}
	e.Tickets += want
	e.Spent += cost
	if err := g.bot.Store.Set(ctx, key, e, 0); err != nil {
		// They paid, so give the points back rather than keep them for nothing.
		g.bot.Links.Call(ctx, pointsAdd, map[string]any{"user_id": u.ID, "amount": cost, "reason": "giveaway refund"}, nil)
		return err
	}
	return nil // no reply per purchase: with many buyers the replies would queue behind the chat rate limit
}

// eligible says why u can't enter, or "" if they can.
func (g *giveaways) eligible(ctx *luna.Ctx, ga *Giveaway, u *luna.User) (string, error) {
	switch {
	case u.Rank == "broadcaster":
		return "You can't enter your own giveaway.", nil
	case g.cfg.ExcludeMods && u.IsMod():
		return "Mods can't enter giveaways here.", nil
	case ga.Who == "subs" && !u.Has("sub"):
		return "This giveaway is for subscribers only.", nil
	}
	if ga.Who == "followers" {
		at, err := g.bot.Twitch.FollowedAt(ctx, u.ID)
		if err != nil {
			return "", err
		}
		if at.IsZero() {
			return "This giveaway is for followers only.", nil
		}
		if g.bot.Now().Sub(at) < time.Duration(ga.MinFollowD)*24*time.Hour {
			return fmt.Sprintf("This giveaway needs you to have followed for %d days.", ga.MinFollowD), nil
		}
	}
	if g.cfg.MinAccountDays > 0 {
		tu, err := g.bot.Twitch.User(ctx, u.ID)
		if err != nil {
			return "", err
		}
		if tu == nil || g.bot.Now().Sub(tu.CreatedAt) < time.Duration(g.cfg.MinAccountDays)*24*time.Hour {
			return "Your account is too new to enter.", nil
		}
	}
	var won bool
	if _, err := g.bot.Store.Get(ctx, "won:"+u.ID, &won); err != nil {
		return "", err
	} else if won {
		return "You won recently, so sit this one out. 💜", nil
	}
	return "", nil
}

// luck is how many tickets a keyword entry gets: one, plus extras for subs and VIPs, capped.
// shortcut: subs get the same luck at every tier, since chat badges don't carry the tier; read it from the sub events if streamers ask.
func (g *giveaways) luck(u *luna.User) int {
	n := 1
	if u.Has("sub") {
		n += g.cfg.SubLuck
	}
	if u.Has("vip") {
		n += g.cfg.VIPLuck
	}
	return max(1, min(n, g.cfg.LuckCap))
}

func (g *giveaways) entries(ctx *luna.Ctx, gid int64) ([]Entrant, error) {
	keys, err := g.bot.Store.Keys(ctx, fmt.Sprintf("e:%d:", gid))
	if err != nil {
		return nil, err
	}
	out := []Entrant{}
	for _, k := range keys {
		var e Entrant
		if ok, err := g.bot.Store.Get(ctx, k, &e); err != nil {
			return nil, err
		} else if ok && e.Tickets > 0 {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b Entrant) int { return strings.Compare(a.UserID, b.UserID) })
	return out, nil
}

// ---------- drawing ----------

// Pick is the draw, exactly as `luna giveaways verify` and anyone else can re-run it:
// entries sorted by user ID, previous winners left out, then ticket number
// SHA-256(seed ‖ "draw:" ‖ n) mod total tickets, counted through the entries in order.
func Pick(seed []byte, n int, entries []Entrant, exclude []string) (Entrant, bool) {
	var total int64
	var pool []Entrant
	for _, e := range entries {
		if !slices.Contains(exclude, e.UserID) && e.Tickets > 0 {
			pool = append(pool, e)
			total += int64(e.Tickets)
		}
	}
	if total == 0 {
		return Entrant{}, false
	}
	h := sha256.Sum256(append(slices.Clone(seed), fmt.Sprintf("draw:%d", n)...))
	t := new(big.Int).Mod(new(big.Int).SetBytes(h[:]), big.NewInt(total)).Int64()
	for _, e := range pool {
		if t < int64(e.Tickets) {
			return e, true
		}
		t -= int64(e.Tickets)
	}
	panic("unreachable")
}

func (g *giveaways) draw(ctx *luna.Ctx, reroll bool) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ga := g.cur
	if ga == nil || ga.Cancelled {
		return "", errors.New("There's no giveaway to draw.")
	}
	if !reroll && len(ga.Draws) > 0 {
		return "", errors.New("Already drawn: use !giveaway reroll to pick someone else.")
	}
	if reroll && len(ga.Draws) == 0 {
		return "", errors.New("Nothing to reroll yet: !giveaway draw first.")
	}
	if ga.Open { // drawing closes entries, so the list is frozen
		ga.Open, ga.Closed = false, g.bot.Now()
		if g.closeT != nil {
			g.closeT()
			g.closeT = nil
		}
	}
	es, err := g.entries(ctx, ga.ID)
	if err != nil {
		return "", err
	}
	seed, _ := hex.DecodeString(ga.Seed)
	var exclude []string
	for _, d := range ga.Draws {
		exclude = append(exclude, d.UserID)
	}
	n := len(ga.Draws) + 1
	w, ok := Pick(seed, n, es, exclude)
	if !ok {
		g.save(ctx, ga)
		return "", errors.New("Nobody (else) is entered.")
	}
	if g.claimT != nil {
		g.claimT()
	}
	ga.Draws = append(ga.Draws, Draw{N: n, UserID: w.UserID, Login: w.Login, At: g.bot.Now()})
	if err := g.save(ctx, ga); err != nil {
		return "", err
	}
	var b luna.Batch
	b.Set(winKey(ga.ID, n), Win{Draw: ga.Draws[n-1], Prize: ga.Prize, Giveaway: ga.ID}, 0)
	if g.cfg.WinnerCooldown > 0 {
		b.Set("won:"+w.UserID, true, time.Duration(g.cfg.WinnerCooldown)*24*time.Hour)
	}
	if err := g.bot.Store.Apply(ctx, &b); err != nil {
		return "", err
	}
	claim := time.Duration(g.cfg.ClaimS) * time.Second
	g.claimT = g.bot.Timers.After(ctx, claim, func(ctx *luna.Ctx) error {
		g.mu.Lock()
		g.claimT = nil
		g.mu.Unlock()
		return g.bot.Chat.Send(ctx, fmt.Sprintf("%s didn't claim in time. A mod can !giveaway reroll.", w.Login))
	})
	return render(g.cfg.WinnerMessage, map[string]string{"prize": ga.Prize, "user": w.Login, "claim": claim.String(),
		"tickets": strconv.Itoa(w.Tickets), "seed": ga.Seed}), nil
}

func (g *giveaways) verifyMsg(ctx *luna.Ctx) (string, error) {
	e, err := g.export(ctx)
	if err != nil {
		return "", err
	}
	if e.Giveaway.Seed == "" {
		return fmt.Sprintf("Seed commitment: %s. The seed is revealed at the draw.", e.Giveaway.Commit), nil
	}
	return fmt.Sprintf("Seed %s (its SHA-256 is the commitment %s…). %d entries, list digest %s. The streamer can share the full list with: luna giveaways export",
		e.Giveaway.Seed, e.Giveaway.Commit[:16], len(e.Entries), e.Digest[:16]), nil
}

// ---------- export and verify ----------

type Export struct {
	Giveaway  Giveaway  `json:"giveaway"`
	Entries   []Entrant `json:"entries"`
	Digest    string    `json:"entries_digest"` // SHA-256 of "user_id:tickets\n" per entry, in order
	Algorithm string    `json:"algorithm"`
}

func (g *giveaways) export(ctx *luna.Ctx) (*Export, error) {
	g.mu.Lock()
	ga := g.cur
	g.mu.Unlock()
	if ga == nil {
		return nil, errors.New("no giveaway has run yet")
	}
	es, err := g.entries(ctx, ga.ID)
	if err != nil {
		return nil, err
	}
	out := &Export{Giveaway: *ga, Entries: es, Digest: digest(es),
		Algorithm: `entries sorted by user_id; for draw n, leave out earlier winners and pick ticket SHA-256(seed bytes || "draw:<n>") mod total tickets, counting through the entries in order`}
	if ga.Open || len(ga.Draws) == 0 {
		out.Giveaway.Seed = "" // secret until the draw
	}
	return out, nil
}

func digest(es []Entrant) string {
	h := sha256.New()
	for _, e := range es {
		fmt.Fprintf(h, "%s:%d\n", e.UserID, e.Tickets)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Verify re-runs an export's draws and checks the seed against its commitment.
func Verify(e *Export) (string, error) {
	ga := e.Giveaway
	if ga.Seed == "" {
		return "", errors.New("nothing to verify until the draw reveals the seed")
	}
	seed, err := hex.DecodeString(ga.Seed)
	if err != nil {
		return "", err
	}
	if sum := sha256.Sum256(seed); hex.EncodeToString(sum[:]) != ga.Commit {
		return "", errors.New("the seed doesn't match the commitment posted at the start")
	}
	var exclude []string
	for _, d := range ga.Draws {
		w, ok := Pick(seed, d.N, e.Entries, exclude)
		if !ok || w.UserID != d.UserID {
			return "", fmt.Errorf("draw %d: the seed picks %s, but %s was announced", d.N, w.Login, d.Login)
		}
		exclude = append(exclude, d.UserID)
	}
	return fmt.Sprintf("OK: the seed matches its commitment and all %d draws re-run to the same winners.", len(ga.Draws)), nil
}

type Win struct {
	Draw
	Prize     string    `json:"prize"`
	Giveaway  int64     `json:"giveaway"`
	FirstSeen time.Time `json:"first_seen,omitzero"`
	Messages  int64     `json:"messages,omitempty"`
	WatchH    float64   `json:"watch_hours,omitempty"`
}

func (g *giveaways) winners(ctx *luna.Ctx, user string) ([]Win, error) {
	keys, err := g.bot.Store.Keys(ctx, "win:")
	if err != nil {
		return nil, err
	}
	slices.Reverse(keys)
	out := []Win{}
	for _, k := range keys {
		var w Win
		if ok, err := g.bot.Store.Get(ctx, k, &w); err != nil || !ok {
			return nil, err
		}
		if user != "" && !strings.EqualFold(strings.TrimPrefix(user, "@"), w.Login) {
			continue
		}
		if v, err := g.bot.Viewers.Get(ctx, w.UserID); err == nil && v != nil {
			w.FirstSeen, w.Messages, w.WatchH = v.FirstSeen, v.Messages, float64(v.WatchS)/3600
		}
		if out = append(out, w); len(out) == 50 {
			break
		}
	}
	return out, nil
}

func render(tmpl string, vars map[string]string) string {
	var pairs []string
	for k, v := range vars {
		pairs = append(pairs, "{"+k+"}", v)
	}
	return strings.NewReplacer(pairs...).Replace(tmpl)
}
