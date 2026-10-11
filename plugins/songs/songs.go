// Package songs is luna.songs: song requests to the streamer's Spotify, with limits per
// viewer, a maximum length, a blocklist, an optional playlist allowlist (to stay DMCA-safe),
// and an optional points cost or channel point redeem.
package songs

import (
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	luna "luna/sdk/go"
)

//go:embed plugin.toml
var Manifest []byte

type Settings struct {
	LiveOnly     bool   `luna:"live_only"`
	ChatRequests bool   `luna:"chat_requests"`
	Reward       string `luna:"reward"`
	Cost         int    `luna:"cost"`
	MaxPerUser   int    `luna:"max_per_user"`
	MaxLengthS   int    `luna:"max_length_s"`
	NoExplicit   bool   `luna:"no_explicit"`
	Blocklist    string `luna:"blocklist"`
	Playlist     string `luna:"playlist"`
}

// Request is a song a viewer asked for that hasn't finished yet.
type Request struct {
	UserID  string    `json:"user_id"`
	Login   string    `json:"login"`
	TrackID string    `json:"track_id"`
	Title   string    `json:"title"`
	Paid    int64     `json:"paid,omitempty"`
	At      time.Time `json:"at"`
	Wrong   bool      `json:"wrong,omitempty"` // !wrongsong: skip it when it comes up
}

const (
	spend = "luna.points:spend"
	add   = "luna.points:add"
)

type songs struct {
	bot *luna.Bot
	cfg Settings

	mu      sync.Mutex
	live    bool
	pending []Request
	current *Request // the request playing now, if the song playing is one
}

func New() *luna.Bot {
	bot := luna.New()
	s := &songs{bot: bot}
	bot.UseConfig(&s.cfg)
	bot.Links.Use(spend, add)
	bot.OnStart(func(ctx *luna.Ctx) error {
		if st, err := bot.Twitch.Stream(ctx, ""); err == nil {
			s.live = st.Live
		}
		if _, err := bot.Store.Get(ctx, "pending", &s.pending); err != nil {
			return err
		}
		bot.Timers.Every(15*time.Second, s.watch)
		return nil
	})
	bot.On("twitch.stream.online", func(*luna.Ctx, *luna.Event) error { s.setLive(true); return nil })
	bot.On("twitch.stream.offline", func(ctx *luna.Ctx, _ *luna.Event) error {
		s.mu.Lock()
		s.live, s.pending = false, nil // Spotify's queue doesn't outlive the session; neither do the requests
		s.mu.Unlock()
		return s.save(ctx)
	})
	bot.On("twitch.channel.redeem", func(ctx *luna.Ctx, ev *luna.Event) error {
		if s.cfg.Reward == "" || !strings.EqualFold(ev.Str("reward"), s.cfg.Reward) {
			return nil
		}
		msg := s.request(ctx, ev.User(), ev.Str("input"), false)
		return bot.Chat.Send(ctx, msg)
	})
	bot.ChatCommand(luna.ChatCommand{Name: "sr", Aliases: []string{"songrequest"}, UserCooldown: "10s"}, func(ctx *luna.Ctx, args []string) error {
		if !s.cfg.ChatRequests {
			if s.cfg.Reward != "" {
				return ctx.Reply(fmt.Sprintf("Request songs with the %q channel point reward.", s.cfg.Reward))
			}
			return nil
		}
		return ctx.Reply(s.request(ctx, ctx.User(), strings.Join(args, " "), true))
	})
	bot.ChatCommand(luna.ChatCommand{Name: "song", Cooldown: "10s"}, s.song)
	bot.ChatCommand(luna.ChatCommand{Name: "wrongsong", UserCooldown: "5s"}, s.wrong)
	bot.ChatCommand(luna.ChatCommand{Name: "skip", Capability: "songs.skip"}, func(ctx *luna.Ctx, _ []string) error {
		if err := bot.Spotify.Skip(ctx); err != nil {
			return ctx.Reply("Couldn't skip: " + reason(err))
		}
		return nil
	})
	bot.CLI(luna.CLICommand{Name: "list", Summary: "requests waiting to play"},
		func(ctx *luna.Ctx, _ luna.Args) (any, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			return append([]Request{}, s.pending...), nil
		})
	return bot
}

// Main runs the plugin as a capsule: `luna __songs` inside the sandbox.
func Main() { New().Run() }

func reason(err error) string { return strings.TrimPrefix(err.Error(), "luna: ") }

func (s *songs) setLive(v bool) {
	s.mu.Lock()
	s.live = v
	s.mu.Unlock()
}

func (s *songs) save(ctx *luna.Ctx) error {
	s.mu.Lock()
	p := slices.Clone(s.pending)
	s.mu.Unlock()
	return s.bot.Store.Set(ctx, "pending", p, 0)
}

// request checks a request against the rules, charges for it and queues it; the reply says how it went.
func (s *songs) request(ctx *luna.Ctx, u *luna.User, query string, charge bool) string {
	query = strings.TrimSpace(query)
	s.mu.Lock()
	live := s.live
	mine := 0
	for _, r := range s.pending {
		if r.UserID == u.ID && !r.Wrong {
			mine++
		}
	}
	s.mu.Unlock()
	switch {
	case query == "":
		return "Usage: !sr <song name or Spotify link>"
	case s.cfg.LiveOnly && !live:
		return "Song requests open when the stream is live."
	case s.cfg.MaxPerUser > 0 && mine >= s.cfg.MaxPerUser:
		return fmt.Sprintf("%s, you already have %d songs waiting.", u.DisplayName(), mine)
	}
	t, err := s.bot.Spotify.Find(ctx, query)
	switch {
	case err != nil:
		return "Spotify isn't answering: " + reason(err)
	case t == nil:
		return fmt.Sprintf("No song found for %q.", query)
	}
	title := t.Name + " by " + t.Artists
	if why := s.refused(ctx, t); why != "" {
		return fmt.Sprintf("Can't request %s: %s.", title, why)
	}
	var paid int64
	if charge && s.cfg.Cost > 0 {
		paid = int64(s.cfg.Cost)
		err := s.bot.Links.Call(ctx, spend, map[string]any{"user_id": u.ID, "login": u.Login, "amount": paid, "reason": "song request"}, nil)
		switch {
		case errors.Is(err, luna.ErrLinkUnavailable):
			return "Song requests cost points, and the Points plugin isn't running."
		case err != nil:
			return strings.TrimPrefix(err.Error(), spend+": ")
		}
	}
	if err := s.bot.Spotify.Queue(ctx, t.URI); err != nil {
		s.refund(ctx, u.ID, u.Login, paid)
		return "Spotify refused: " + reason(err) // most often: no active player, or not Premium
	}
	if ctx.Event != nil && ctx.Event.Simulated {
		return fmt.Sprintf("🎵 Added %s (simulated: not queued on Spotify).", title) // nothing to wait for
	}
	s.mu.Lock()
	s.pending = append(s.pending, Request{UserID: u.ID, Login: u.Login, TrackID: t.ID, Title: title, Paid: paid, At: s.bot.Now()})
	n := len(s.pending)
	s.mu.Unlock()
	if err := s.save(ctx); err != nil {
		s.bot.Log.Warn(ctx, "song request not saved: "+err.Error())
	}
	return fmt.Sprintf("🎵 Added %s (#%d in the queue).", title, n)
}

// refused says why a track breaks the rules, or "" if it doesn't.
func (s *songs) refused(ctx *luna.Ctx, t *luna.Track) string {
	if s.cfg.MaxLengthS > 0 && t.DurationMS > s.cfg.MaxLengthS*1000 {
		return fmt.Sprintf("it's longer than %s", time.Duration(s.cfg.MaxLengthS)*time.Second)
	}
	if s.cfg.NoExplicit && t.Explicit {
		return "it's explicit"
	}
	text := strings.ToLower(t.Name + " " + t.Artists + " " + t.ID)
	for _, b := range strings.Split(s.cfg.Blocklist, ",") {
		if b = strings.ToLower(strings.TrimSpace(b)); b != "" && strings.Contains(text, b) {
			return "it's on the blocklist"
		}
	}
	if s.cfg.Playlist != "" {
		in, err := s.bot.Spotify.InPlaylist(ctx, s.cfg.Playlist, t.ID)
		if err != nil {
			return "the request playlist can't be read right now"
		}
		if !in {
			return "only songs from the request playlist are allowed"
		}
	}
	return ""
}

func (s *songs) refund(ctx *luna.Ctx, uid, login string, amount int64) {
	if amount <= 0 {
		return
	}
	if err := s.bot.Links.Call(ctx, add, map[string]any{"user_id": uid, "login": login, "amount": amount, "reason": "song request refund"}, nil); err != nil {
		s.bot.Log.Warn(ctx, fmt.Sprintf("couldn't refund %s's %d points: %v", login, amount, err))
	}
}

func (s *songs) wrong(ctx *luna.Ctx, _ []string) error {
	u := ctx.User()
	s.mu.Lock()
	var r *Request
	for i := len(s.pending) - 1; i >= 0; i-- {
		if s.pending[i].UserID == u.ID && !s.pending[i].Wrong {
			r = &s.pending[i]
			break
		}
	}
	var title string
	var paid int64
	if r != nil {
		r.Wrong, title, paid = true, r.Title, r.Paid
		r.Paid = 0
	}
	s.mu.Unlock()
	if r == nil {
		return ctx.Reply("You have no song waiting.")
	}
	s.refund(ctx, u.ID, u.Login, paid)
	if err := s.save(ctx); err != nil {
		return err
	}
	// shortcut: Spotify can't remove a song from the queue, so it's skipped when it starts; drop this if Spotify adds removal.
	return ctx.Reply(fmt.Sprintf("Got it: %s will be skipped when it comes up.", title))
}

func (s *songs) song(ctx *luna.Ctx, _ []string) error {
	p, err := s.bot.Spotify.Playing(ctx)
	if err != nil {
		return ctx.Reply("Spotify isn't answering: " + reason(err))
	}
	if p.Track == nil {
		return ctx.Reply("Nothing is playing.")
	}
	msg := "🎵 Now playing: " + p.Track.Name + " by " + p.Track.Artists
	s.mu.Lock()
	if s.current != nil && s.current.TrackID == p.Track.ID {
		msg += " (requested by " + s.current.Login + ")"
	}
	s.mu.Unlock()
	return ctx.Reply(msg)
}

// watch notices when a requested song starts: it leaves the waiting list, and a !wrongsong'd one is skipped.
func (s *songs) watch(ctx *luna.Ctx) error {
	s.mu.Lock()
	n := len(s.pending)
	// A request that never came up (skipped in Spotify, its queue cleared) stops counting after 3 hours.
	s.pending = slices.DeleteFunc(s.pending, func(r Request) bool { return s.bot.Now().Sub(r.At) > 3*time.Hour })
	expired, idle := len(s.pending) != n, len(s.pending) == 0
	s.mu.Unlock()
	if expired {
		if err := s.save(ctx); err != nil {
			return err
		}
	}
	if idle {
		return nil
	}
	p, err := s.bot.Spotify.Playing(ctx)
	if err != nil || p.Track == nil {
		return nil // Spotify asleep or offline: try again next time
	}
	s.mu.Lock()
	i := slices.IndexFunc(s.pending, func(r Request) bool { return r.TrackID == p.Track.ID })
	if i < 0 {
		s.mu.Unlock()
		return nil
	}
	r := s.pending[i]
	s.pending = slices.Delete(s.pending, i, i+1)
	s.current = &r
	s.mu.Unlock()
	if err := s.save(ctx); err != nil {
		return err
	}
	if r.Wrong {
		if err := s.bot.Spotify.Skip(ctx); err != nil {
			return err
		}
		return s.bot.Chat.Send(ctx, "Skipped "+r.Title+" (wrong song).")
	}
	return nil
}
