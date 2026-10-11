// Package stream is luna.stream, Stream tools: title and category, clips and markers, ad
// warnings, the schedule, a raid helper and an after-stream recap.
package stream

import (
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // the sandbox has no /usr/share/zoneinfo, and !schedule shows viewers' own time zones

	luna "luna/sdk/go"
)

//go:embed plugin.toml
var Manifest []byte

type Settings struct {
	AdWarn         bool   `luna:"ad_warn"`
	AdWarnS        int    `luna:"ad_warn_s"`
	AdWarnMessage  string `luna:"ad_warn_message"`
	AdThanks       bool   `luna:"ad_thanks"`
	AdThanksMsg    string `luna:"ad_thanks_message"`
	ClipMessage    string `luna:"clip_message"`
	RaidFriends    string `luna:"raid_friends"`
	ScheduleZone   string `luna:"schedule_timezone"`
	ViewerPollM    int    `luna:"viewer_poll_m"`
	RecapTopChatrs int    `luna:"recap_top_chatters"`
}

// Recap is one stream, gathered as it happens and kept after it ends.
type Recap struct {
	Started     time.Time        `json:"started"`
	Ended       time.Time        `json:"ended,omitzero"`
	Game        string           `json:"game,omitempty"`
	Followers   []string         `json:"followers"`
	Subs        int              `json:"subs"`
	GiftedSubs  int              `json:"gifted_subs"`
	Bits        int              `json:"bits"`
	Raids       []string         `json:"raids"`
	Clips       []string         `json:"clips"`
	Markers     []string         `json:"markers"`
	PeakViewers int              `json:"peak_viewers"`
	Messages    int              `json:"messages"`
	Chatters    map[string]int   `json:"chatters"` // login → messages
	TopChatters []map[string]any `json:"top_chatters,omitempty"`
}

type stream struct {
	bot *luna.Bot
	cfg Settings

	mu      sync.Mutex
	live    bool
	recap   *Recap
	pending map[string]luna.Category // who asked for a category → the one we suggested
	adT     func()
	adAt    time.Time
}

func New() *luna.Bot {
	bot := luna.New()
	s := &stream{bot: bot, pending: map[string]luna.Category{}}
	bot.UseConfig(&s.cfg)
	bot.OnStart(func(ctx *luna.Ctx) error {
		var r Recap
		if ok, err := bot.Store.Get(ctx, "recap:current", &r); err != nil {
			return err
		} else if ok {
			s.recap = &r
		}
		if st, err := bot.Twitch.Stream(ctx, ""); err == nil && st.Live {
			s.online(ctx, st.StartedAt)
		}
		bot.Timers.Every(time.Duration(max(s.cfg.ViewerPollM, 1))*time.Minute, s.poll)
		return nil
	})
	bot.On("twitch.stream.online", func(ctx *luna.Ctx, ev *luna.Event) error {
		s.online(ctx, bot.Now())
		return s.poll(ctx)
	})
	bot.On("twitch.stream.offline", s.offline)
	bot.On("twitch.channel.ad_break", s.adBreak)
	bot.On("twitch.channel.update", func(ctx *luna.Ctx, ev *luna.Event) error {
		return s.track(func(r *Recap) { r.Game = ev.Str("game") })
	})
	bot.On("twitch.chat.message", func(ctx *luna.Ctx, ev *luna.Event) error {
		u := ev.User()
		return s.track(func(r *Recap) {
			r.Messages++
			// shortcut: counts the first 2000 chatters, so the recap stays under the store's 64 KiB per value; keep counts per key if bigger channels need all of them.
			if u == nil || u.Rank == "broadcaster" {
				return
			}
			if _, seen := r.Chatters[u.Login]; seen || len(r.Chatters) < 2000 {
				r.Chatters[u.Login]++
			}
		})
	})
	bot.On("twitch.channel.follow", func(ctx *luna.Ctx, ev *luna.Event) error {
		return s.track(func(r *Recap) { r.Followers = append(r.Followers, ev.User().Login) })
	})
	bot.On("twitch.channel.subscribe", func(ctx *luna.Ctx, ev *luna.Event) error {
		return s.track(func(r *Recap) {
			if !ev.Bool("gift") {
				r.Subs++
			}
		})
	})
	bot.On("twitch.channel.resub", func(ctx *luna.Ctx, ev *luna.Event) error { return s.track(func(r *Recap) { r.Subs++ }) })
	bot.On("twitch.channel.gift", func(ctx *luna.Ctx, ev *luna.Event) error {
		return s.track(func(r *Recap) { r.GiftedSubs += int(ev.Int("count")) })
	})
	bot.On("twitch.channel.cheer", func(ctx *luna.Ctx, ev *luna.Event) error {
		return s.track(func(r *Recap) { r.Bits += int(ev.Int("bits")) })
	})
	bot.On("twitch.channel.raid", func(ctx *luna.Ctx, ev *luna.Event) error {
		return s.track(func(r *Recap) {
			r.Raids = append(r.Raids, fmt.Sprintf("%s (%d)", ev.User().Login, ev.Int("viewers")))
		})
	})

	bot.ChatCommand(luna.ChatCommand{Name: "clip", Cooldown: "30s"}, s.clip)
	bot.ChatCommand(luna.ChatCommand{Name: "marker", Capability: "stream.edit"}, s.marker)
	bot.ChatCommand(luna.ChatCommand{Name: "ads", Capability: "stream.edit"}, s.ads)
	bot.ChatCommand(luna.ChatCommand{Name: "schedule", UserCooldown: "10s"}, s.schedule)
	bot.ChatCommand(luna.ChatCommand{Name: "raid", Capability: "stream.edit"}, s.raid)

	// !title and !game belong to the core bundle, which hands edits by Luna mods over this link.
	bot.Links.Provide("edit", func(ctx *luna.Ctx, caller string, a luna.Args) (any, error) {
		msg, err := s.edit(ctx, a.Str("what"), a.Str("value"), a.Str("actor"), a.Bool("simulated"))
		if err != nil {
			return nil, err
		}
		return map[string]string{"message": msg}, nil
	})

	bot.CLI(luna.CLICommand{Name: "title", Summary: "set the stream title", Args: []luna.Arg{{Name: "text", Type: "str"}}, Mutates: true},
		func(ctx *luna.Ctx, a luna.Args) (any, error) {
			return s.edit(ctx, "title", a.Str("text"), "cli", false)
		})
	bot.CLI(luna.CLICommand{Name: "game", Summary: "set the category; an inexact name lists the closest matches",
		Args: []luna.Arg{{Name: "name", Type: "str"}}, Mutates: true},
		func(ctx *luna.Ctx, a luna.Args) (any, error) { return s.edit(ctx, "game", a.Str("name"), "cli", false) })
	bot.CLI(luna.CLICommand{Name: "recap", Summary: "the current or last stream: followers, subs, top chatters, clips, markers, peak viewers"},
		func(ctx *luna.Ctx, _ luna.Args) (any, error) { return s.lastRecap(ctx) })
	return bot
}

// Main runs the plugin as a capsule: `luna __stream` inside the sandbox.
func Main() { New().Run() }

// ---------- recap ----------

func newRecap(at time.Time) *Recap {
	return &Recap{Started: at, Followers: []string{}, Raids: []string{}, Clips: []string{}, Markers: []string{}, Chatters: map[string]int{}}
}

func (s *stream) online(ctx *luna.Ctx, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live = true
	if s.recap == nil || !s.recap.Ended.IsZero() || at.Sub(s.recap.Started) > time.Hour {
		s.recap = newRecap(at) // a restart mid-stream keeps the recap it was building
	}
}

// track updates the live recap; the poll saves it.
func (s *stream) track(f func(*Recap)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live && s.recap != nil {
		f(s.recap)
	}
	return nil
}

// poll runs every few minutes while live: peak viewers, the ad schedule, and a save.
func (s *stream) poll(ctx *luna.Ctx) error {
	s.mu.Lock()
	live := s.live
	s.mu.Unlock()
	if !live {
		return nil
	}
	st, err := s.bot.Twitch.Stream(ctx, "")
	if err != nil {
		return err
	}
	s.mu.Lock()
	if st.Viewers > s.recap.PeakViewers {
		s.recap.PeakViewers = st.Viewers
	}
	if st.Game != "" && s.recap.Game == "" {
		s.recap.Game = st.Game
	}
	r := *s.recap
	s.mu.Unlock()
	if err := s.bot.Store.Set(ctx, "recap:current", r, 0); err != nil {
		return err
	}
	if s.cfg.AdWarn {
		return s.scheduleAdWarning(ctx)
	}
	return nil
}

func (s *stream) offline(ctx *luna.Ctx, ev *luna.Event) error {
	s.mu.Lock()
	if !s.live || s.recap == nil {
		s.live = false
		s.mu.Unlock()
		return nil
	}
	s.live = false
	s.recap.Ended = s.bot.Now()
	r := *s.recap
	if s.adT != nil {
		s.adT()
		s.adT = nil
	}
	s.mu.Unlock()
	r.TopChatters = s.top(r.Chatters)
	var b luna.Batch
	b.Set("recap:current", r, 0)
	b.Set("recap:last", r, 0)
	if err := s.bot.Store.Apply(ctx, &b); err != nil {
		return err
	}
	s.bot.Log.Info(ctx, fmt.Sprintf("stream recap: %s live, %d new followers, %d subs, %d gifted, %d bits, peak %d viewers, %d clips, %d markers; luna stream recap for the rest",
		r.Ended.Sub(r.Started).Round(time.Minute), len(r.Followers), r.Subs, r.GiftedSubs, r.Bits, r.PeakViewers, len(r.Clips), len(r.Markers)))
	return nil
}

func (s *stream) top(chatters map[string]int) []map[string]any {
	type kv struct {
		login string
		n     int
	}
	var all []kv
	for l, n := range chatters {
		all = append(all, kv{l, n})
	}
	slices.SortFunc(all, func(a, b kv) int {
		if a.n != b.n {
			return b.n - a.n
		}
		return strings.Compare(a.login, b.login)
	})
	out := []map[string]any{}
	for _, c := range all[:min(len(all), max(s.cfg.RecapTopChatrs, 0))] {
		out = append(out, map[string]any{"login": c.login, "messages": c.n})
	}
	return out
}

func (s *stream) lastRecap(ctx *luna.Ctx) (*Recap, error) {
	s.mu.Lock()
	if s.live && s.recap != nil {
		r := *s.recap
		s.mu.Unlock()
		r.TopChatters = s.top(r.Chatters)
		r.Chatters = nil
		return &r, nil
	}
	s.mu.Unlock()
	var r Recap
	if ok, err := s.bot.Store.Get(ctx, "recap:last", &r); err != nil {
		return nil, err
	} else if !ok {
		return nil, errors.New("no stream recorded yet; recaps start with the next stream")
	}
	r.Chatters = nil
	return &r, nil
}

// ---------- title and category ----------

func (s *stream) edit(ctx *luna.Ctx, what, value, actor string, simulated bool) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s needs a value", what)
	}
	set := func(title, gameID, done string) (string, error) {
		if simulated {
			return done + " (simulated: Twitch wasn't changed)", nil
		}
		if err := s.bot.Twitch.SetChannel(ctx, title, gameID); err != nil {
			return "", err
		}
		return done, nil
	}
	switch what {
	case "title":
		if len([]rune(value)) > 140 {
			return "", errors.New("Twitch titles are at most 140 characters.")
		}
		return set(value, "", "Title set: "+value)
	case "game":
		s.mu.Lock()
		p, ok := s.pending[actor]
		delete(s.pending, actor)
		s.mu.Unlock()
		if ok && strings.EqualFold(value, "yes") {
			return set("", p.ID, "Category set: "+p.Name)
		}
		cats, err := s.bot.Twitch.SearchCategories(ctx, value)
		if err != nil {
			return "", err
		}
		if len(cats) == 0 {
			return "", fmt.Errorf("No Twitch category matches %q.", value)
		}
		for _, c := range cats {
			if strings.EqualFold(c.Name, value) {
				return set("", c.ID, "Category set: "+c.Name)
			}
		}
		s.mu.Lock()
		s.pending[actor] = cats[0]
		s.mu.Unlock()
		if actor == "cli" {
			var names []string
			for _, c := range cats[:min(len(cats), 5)] {
				names = append(names, strconv.Quote(c.Name))
			}
			return "", fmt.Errorf("no exact match; closest: %s", strings.Join(names, ", "))
		}
		return fmt.Sprintf("Did you mean %s? Type !game yes to set it.", cats[0].Name), nil
	}
	return "", fmt.Errorf("can't edit %q", what)
}

// ---------- clips and markers ----------

func (s *stream) isLive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live
}

func (s *stream) clip(ctx *luna.Ctx, _ []string) error {
	if !s.isLive() {
		return ctx.Reply("Clips only work while the stream is live.")
	}
	cl, err := s.bot.Twitch.Clip(ctx)
	if err != nil {
		return ctx.Reply("Twitch couldn't make a clip: " + err.Error())
	}
	s.track(func(r *Recap) { r.Clips = append(r.Clips, cl.URL) })
	return ctx.Reply(strings.ReplaceAll(s.cfg.ClipMessage, "{url}", cl.URL))
}

func (s *stream) marker(ctx *luna.Ctx, args []string) error {
	if !s.isLive() {
		return ctx.Reply("Markers only work while the stream is live.")
	}
	note := strings.Join(args, " ")
	if len([]rune(note)) > 140 {
		note = string([]rune(note)[:140])
	}
	m, err := s.bot.Twitch.Marker(ctx, note)
	if err != nil {
		return ctx.Reply("Twitch couldn't add a marker: " + err.Error())
	}
	at := (time.Duration(m.PositionS) * time.Second).String()
	s.track(func(r *Recap) { r.Markers = append(r.Markers, strings.TrimSpace(at+" "+note)) })
	return ctx.Reply("Marker added at " + at + ".")
}

// ---------- ads ----------

func (s *stream) ads(ctx *luna.Ctx, args []string) error {
	if len(args) > 0 && strings.EqualFold(args[0], "snooze") {
		a, err := s.bot.Twitch.SnoozeAd(ctx)
		if err != nil {
			return ctx.Reply("Couldn't snooze: " + err.Error())
		}
		s.mu.Lock()
		s.adAt = time.Time{} // re-arm the warning for the new time
		s.mu.Unlock()
		if s.cfg.AdWarn {
			s.scheduleAdWarning(ctx)
		}
		return ctx.Reply(fmt.Sprintf("Snoozed: the next ad is %s. %d snoozes left.", s.until(a.NextAdAt), a.SnoozeCount))
	}
	a, err := s.bot.Twitch.Ads(ctx)
	if err != nil {
		return err
	}
	if a.NextAdAt.IsZero() {
		return ctx.Reply("No ad is scheduled.")
	}
	return ctx.Reply(fmt.Sprintf("Next ad %s (%ds). %d snoozes left: !ads snooze", s.until(a.NextAdAt), a.DurationS, a.SnoozeCount))
}

func (s *stream) until(t time.Time) string {
	d := t.Sub(s.bot.Now()).Round(time.Second)
	if d <= 0 {
		return "now"
	}
	return "in " + d.String()
}

// scheduleAdWarning arms one timer for ad_warn_s before the next scheduled ad.
func (s *stream) scheduleAdWarning(ctx *luna.Ctx) error {
	a, err := s.bot.Twitch.Ads(ctx)
	if err != nil {
		return err
	}
	warnAt := a.NextAdAt.Add(-time.Duration(s.cfg.AdWarnS) * time.Second)
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.NextAdAt.IsZero() || a.NextAdAt.Equal(s.adAt) || !warnAt.After(s.bot.Now()) {
		return nil
	}
	if s.adT != nil {
		s.adT()
	}
	s.adAt = a.NextAdAt
	s.adT = s.bot.Timers.After(ctx, warnAt.Sub(s.bot.Now()), func(ctx *luna.Ctx) error {
		cur, err := s.bot.Twitch.Ads(ctx)
		if err != nil || !cur.NextAdAt.Equal(a.NextAdAt) || !s.isLive() {
			return err // snoozed or rescheduled since: the poll re-arms for the new time
		}
		return s.bot.Chat.Send(ctx, strings.NewReplacer("{in}", s.until(cur.NextAdAt), "{duration}",
			(time.Duration(cur.DurationS)*time.Second).String()).Replace(s.cfg.AdWarnMessage))
	})
	return nil
}

func (s *stream) adBreak(ctx *luna.Ctx, ev *luna.Event) error {
	if !s.cfg.AdThanks {
		return nil
	}
	s.bot.Timers.After(ctx, time.Duration(ev.Int("duration_s"))*time.Second, func(ctx *luna.Ctx) error {
		return s.bot.Chat.Send(ctx, s.cfg.AdThanksMsg)
	})
	return nil
}

// ---------- schedule ----------

func (s *stream) schedule(ctx *luna.Ctx, args []string) error {
	u := ctx.User()
	if len(args) >= 1 && strings.EqualFold(args[0], "tz") {
		if len(args) < 2 {
			return ctx.Reply("Usage: !schedule tz <zone>, like Europe/London or America/New_York")
		}
		loc, err := time.LoadLocation(args[1])
		if err != nil {
			return ctx.Reply(fmt.Sprintf("%q isn't a time zone I know; try one like Europe/London.", args[1]))
		}
		if err := s.bot.Store.Set(ctx, "tz:"+u.ID, loc.String(), 0); err != nil {
			return err
		}
		return ctx.Reply("Got it: I'll show the schedule in " + loc.String() + " time.")
	}
	segs, err := s.bot.Twitch.Schedule(ctx)
	if err != nil {
		return err
	}
	now := s.bot.Now()
	i := slices.IndexFunc(segs, func(sg luna.Segment) bool { return sg.Start.After(now) })
	if i < 0 {
		return ctx.Reply("No upcoming streams on the schedule.")
	}
	sg := segs[i]
	zone := s.cfg.ScheduleZone
	var mine string
	if _, err := s.bot.Store.Get(ctx, "tz:"+u.ID, &mine); err != nil {
		return err
	} else if mine != "" {
		zone = mine
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.UTC
	}
	when := sg.Start.In(loc).Format("Mon 2 Jan 15:04 MST")
	what := sg.Title
	if sg.Category != "" {
		what = strings.TrimSpace(what + " (" + sg.Category + ")")
	}
	msg := fmt.Sprintf("Next stream: %s, %s", when, s.until(sg.Start))
	if what != "" {
		msg += ": " + what
	}
	if mine == "" {
		msg += ". !schedule tz <zone> to see it in your time."
	}
	return ctx.Reply(msg)
}

// ---------- raids ----------

func (s *stream) raid(ctx *luna.Ctx, args []string) error {
	if len(args) == 0 {
		friends := csv(s.cfg.RaidFriends)
		if len(friends) == 0 {
			return ctx.Reply("No raid friends set: luna config set plugins.stream.raid_friends a,b,c")
		}
		live, err := s.bot.Twitch.Streams(ctx, friends)
		if err != nil {
			return err
		}
		if len(live) == 0 {
			return ctx.Reply("None of your raid friends are live right now.")
		}
		slices.SortFunc(live, func(a, b luna.LiveStream) int { return a.Viewers - b.Viewers }) // smallest first: raids help them most
		var parts []string
		for _, l := range live[:min(len(live), 5)] {
			parts = append(parts, fmt.Sprintf("%s (%s, %d)", l.Login, l.Game, l.Viewers))
		}
		return ctx.Reply("Live friends: " + strings.Join(parts, " · ") + ". !raid <name> to go.")
	}
	if strings.EqualFold(args[0], "cancel") {
		if err := s.bot.Twitch.Unraid(ctx); err != nil {
			return ctx.Reply("Couldn't cancel: " + err.Error())
		}
		return ctx.Reply("Raid cancelled.")
	}
	target := strings.ToLower(strings.TrimPrefix(args[0], "@"))
	u, err := s.bot.Twitch.User(ctx, target)
	if err != nil {
		return err
	}
	if u == nil {
		return ctx.Reply(fmt.Sprintf("No Twitch channel %q.", target))
	}
	if err := s.bot.Twitch.Raid(ctx, u.ID); err != nil {
		return ctx.Reply("Twitch refused the raid: " + err.Error())
	}
	return ctx.Reply(fmt.Sprintf("Raiding %s in 90 seconds! (!raid cancel to stop)", u.Name))
}

func csv(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.ToLower(strings.TrimSpace(f)); f != "" {
			out = append(out, f)
		}
	}
	return out
}
