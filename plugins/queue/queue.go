// Package queue is luna.queue: viewers line up with !join to play with the streamer, and
// mods pull the next group with !next. Subs can go first, players who just had a turn wait
// a while before rejoining, and the group can be whispered an invite.
package queue

import (
	_ "embed"
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
	SubPriority   bool   `luna:"sub_priority"`
	GroupSize     int    `luna:"group_size"`
	RejoinAfterM  int    `luna:"rejoin_after_m"`
	MaxSize       int    `luna:"max_size"`
	InviteMessage string `luna:"invite_message"`
}

type Entry struct {
	UserID string    `json:"user_id"`
	Login  string    `json:"login"`
	Name   string    `json:"name,omitempty"` // in-game username, from !join <name>
	Sub    bool      `json:"sub,omitempty"`
	Joined time.Time `json:"joined"`
}

// State is the whole queue, saved under one store key so a restart keeps it.
// shortcut: one key holds about 900 entries within the store's 64 KiB per value; split it if queues grow past that.
type State struct {
	Open    bool                 `json:"open"`
	Entries []Entry              `json:"entries"`
	Played  map[string]time.Time `json:"played"` // user id → when they were last pulled
}

type queue struct {
	bot *luna.Bot
	cfg Settings
	mu  sync.Mutex
}

func New() *luna.Bot {
	bot := luna.New()
	q := &queue{bot: bot}
	bot.UseConfig(&q.cfg)
	bot.ChatCommand(luna.ChatCommand{Name: "join"}, q.join)
	bot.ChatCommand(luna.ChatCommand{Name: "leave"}, q.leave)
	bot.ChatCommand(luna.ChatCommand{Name: "queue", Cooldown: "5s"}, q.queueCmd)
	bot.ChatCommand(luna.ChatCommand{Name: "next", Capability: "queue.run"}, q.next)
	bot.CLI(luna.CLICommand{Name: "list", Summary: "everyone waiting, in order"},
		func(ctx *luna.Ctx, _ luna.Args) (any, error) {
			var out []Entry
			err := q.update(ctx, func(s *State) error { out = q.order(s); return nil })
			return out, err
		})
	return bot
}

// Main runs the plugin as a capsule: `luna __queue` inside the sandbox.
func Main() { New().Run() }

// update loads the queue, applies f, and saves it if f succeeded.
func (q *queue) update(ctx *luna.Ctx, f func(*State) error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	var s State
	if _, err := q.bot.Store.Get(ctx, "state", &s); err != nil {
		return err
	}
	if s.Played == nil {
		s.Played = map[string]time.Time{}
	}
	if err := f(&s); err != nil {
		return err
	}
	cut := q.bot.Now().Add(-time.Duration(q.cfg.RejoinAfterM) * time.Minute)
	for id, at := range s.Played {
		if at.Before(cut) {
			delete(s.Played, id) // past their wait: nothing to remember
		}
	}
	return q.bot.Store.Set(ctx, "state", s, 0)
}

// order is the queue as it will be pulled: subs first if configured, then by join time.
func (q *queue) order(s *State) []Entry {
	out := slices.Clone(s.Entries)
	slices.SortStableFunc(out, func(a, b Entry) int {
		if q.cfg.SubPriority && a.Sub != b.Sub {
			if a.Sub {
				return -1
			}
			return 1
		}
		return a.Joined.Compare(b.Joined)
	})
	return out
}

func display(e Entry) string {
	if e.Name != "" {
		return e.Login + " (" + e.Name + ")"
	}
	return e.Login
}

func (q *queue) join(ctx *luna.Ctx, args []string) error {
	u := ctx.User()
	var reply string
	err := q.update(ctx, func(s *State) error {
		name := strings.Join(args, " ")
		if len([]rune(name)) > 32 {
			name = string([]rune(name)[:32])
		}
		if i := slices.IndexFunc(s.Entries, func(e Entry) bool { return e.UserID == u.ID }); i >= 0 {
			if name != "" {
				s.Entries[i].Name = name // already in: keep their place, update the name
			}
			return nil
		}
		rejoin := s.Played[u.ID].Add(time.Duration(q.cfg.RejoinAfterM) * time.Minute)
		switch {
		case !s.Open:
			reply = "The queue is closed."
		case q.bot.Now().Before(rejoin):
			wait := rejoin.Sub(q.bot.Now()).Round(time.Minute)
			reply = fmt.Sprintf("%s, you just played: you can rejoin in %s.", u.DisplayName(), max(wait, time.Minute))
		case q.cfg.MaxSize > 0 && len(s.Entries) >= q.cfg.MaxSize:
			reply = "The queue is full."
		default:
			s.Entries = append(s.Entries, Entry{UserID: u.ID, Login: u.Login, Name: name, Sub: u.Has("sub"), Joined: q.bot.Now()})
		}
		return nil
	})
	if err != nil || reply == "" {
		return err // joining is quiet; !queue shows your place
	}
	return ctx.Reply(reply)
}

func (q *queue) leave(ctx *luna.Ctx, _ []string) error {
	u := ctx.User()
	return q.update(ctx, func(s *State) error {
		s.Entries = slices.DeleteFunc(s.Entries, func(e Entry) bool { return e.UserID == u.ID })
		return nil
	})
}

func (q *queue) queueCmd(ctx *luna.Ctx, args []string) error {
	u := ctx.User()
	if len(args) > 0 {
		verb := strings.ToLower(args[0])
		// shortcut: the rank check mirrors queue.run's default (moderator); read the streamer's capability table once the plugin API exposes it.
		if !u.RankAtLeast("moderator") || !slices.Contains([]string{"open", "close", "clear"}, verb) {
			return nil
		}
		msg := map[string]string{"open": "The queue is open: !join to play!", "close": "The queue is closed.",
			"clear": "The queue is empty."}[verb]
		err := q.update(ctx, func(s *State) error {
			switch verb {
			case "open", "close":
				s.Open = verb == "open"
			case "clear":
				s.Entries = nil
			}
			return nil
		})
		if err != nil {
			return err
		}
		return ctx.Reply(msg)
	}
	var msg string
	err := q.update(ctx, func(s *State) error {
		order := q.order(s)
		if len(order) == 0 {
			msg = "Nobody is in the queue."
			if !s.Open {
				msg = "The queue is closed."
			}
			return nil
		}
		var names []string
		for _, e := range order[:min(len(order), 10)] {
			names = append(names, display(e))
		}
		msg = fmt.Sprintf("%d waiting: %s", len(order), strings.Join(names, ", "))
		if len(order) > 10 {
			msg += ", …"
		}
		if i := slices.IndexFunc(order, func(e Entry) bool { return e.UserID == u.ID }); i >= 0 {
			msg = fmt.Sprintf("%s, you're #%d. %s", u.DisplayName(), i+1, msg)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return ctx.Reply(msg)
}

func (q *queue) next(ctx *luna.Ctx, args []string) error {
	n := max(q.cfg.GroupSize, 1)
	if len(args) > 0 {
		v, err := strconv.Atoi(args[0])
		if err != nil || v < 1 || v > 50 {
			return ctx.Reply("Usage: !next [how many, up to 50]")
		}
		n = v
	}
	var group []Entry
	err := q.update(ctx, func(s *State) error {
		order := q.order(s)
		group = order[:min(n, len(order))]
		for _, e := range group {
			s.Played[e.UserID] = q.bot.Now()
			s.Entries = slices.DeleteFunc(s.Entries, func(x Entry) bool { return x.UserID == e.UserID })
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(group) == 0 {
		return ctx.Reply("Nobody is in the queue.")
	}
	var names []string
	for _, e := range group {
		names = append(names, "@"+display(e))
	}
	if q.cfg.InviteMessage != "" {
		for _, e := range group {
			if err := q.bot.Chat.Whisper(ctx, e.UserID, strings.ReplaceAll(q.cfg.InviteMessage, "{name}", e.Login)); err != nil {
				q.bot.Log.Warn(ctx, fmt.Sprintf("invite to %s not whispered: %v", e.Login, err)) // Twitch only lets verified accounts whisper
			}
		}
	}
	return ctx.Reply("🎮 Up next: " + strings.Join(names, ", "))
}
