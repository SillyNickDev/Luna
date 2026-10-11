// Package bundle is Luna's core bundle: Essentials, Moderation, Events and Shoutouts in
// one native capsule (Performance spec), built only on the public Go SDK.
package bundle

import (
	_ "embed"
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	luna "luna/sdk/go"
)

// Manifest is the bundle's plugin.toml; `luna capsule install core` packages it with the luna binary.
//
//go:embed plugin.toml
var Manifest []byte

// Settings mirrors [config] in plugin.toml; luna config set plugins.core.<key> changes them.
type Settings struct {
	CommandsURL       string `luna:"commands_url"`
	LurkMessage       string `luna:"lurk_message"`
	UnlurkMessage     string `luna:"unlurk_message"`
	TimerMinMessages  int    `luna:"timer_min_messages"`
	RegularWatchHours int    `luna:"regular_watch_hours"`
	BirthdayMessage   string `luna:"birthday_message"`

	ModExempt        string `luna:"mod_exempt"`
	ModLadder        string `luna:"mod_ladder"`
	ModStrikeDecayM  int    `luna:"mod_strike_decay_m"`
	ModWarnNative    bool   `luna:"mod_warn_native"`
	ModLinks         bool   `luna:"mod_links"`
	ModLinkAllow     string `luna:"mod_link_allow"`
	ModPermitS       int    `luna:"mod_permit_s"`
	ModPhrases       bool   `luna:"mod_phrases"`
	ModCaps          bool   `luna:"mod_caps"`
	ModCapsPercent   int    `luna:"mod_caps_percent"`
	ModCapsMinLen    int    `luna:"mod_caps_min_len"`
	ModSymbols       bool   `luna:"mod_symbols"`
	ModRepeat        bool   `luna:"mod_repeat"`
	ModRepeatCount   int    `luna:"mod_repeat_count"`
	ModRepeatWindowS int    `luna:"mod_repeat_window_s"`
	ModEmotes        bool   `luna:"mod_emotes"`
	ModEmotesMax     int    `luna:"mod_emotes_max"`
	ModLong          bool   `luna:"mod_long"`
	ModLongMax       int    `luna:"mod_long_max"`
	ModNukeTimeoutS  int    `luna:"mod_nuke_timeout_s"`
	ModShield        bool   `luna:"mod_shield"`
	ModShieldFollows int    `luna:"mod_shield_follows"`
	ModShieldSame    int    `luna:"mod_shield_same_text"`
	ModShieldWindowS int    `luna:"mod_shield_window_s"`
	ModShieldOffM    int    `luna:"mod_shield_off_m"`

	EventsOn           string `luna:"events_on"`
	FollowMessage      string `luna:"follow_message"`
	FollowBatchS       int    `luna:"follow_batch_s"`
	SubMessage         string `luna:"sub_message"`
	ResubMessage       string `luna:"resub_message"`
	GiftMessage        string `luna:"gift_message"`
	BitsMessage        string `luna:"bits_message"`
	RaidMessage        string `luna:"raid_message"`
	FirstMessage       string `luna:"first_message"`
	WelcomeBackMessage string `luna:"welcome_back_message"`
	WelcomeBackDays    int    `luna:"welcome_back_days"`

	SoMessage string `luna:"so_message"`
	SoNative  bool   `luna:"so_native"`
	SoRaids   bool   `luna:"so_raids"`
	SoFriends string `luna:"so_friends"`
}

// core is the state the four parts share. Everything durable is in the store; this is
// what's fine to lose on a restart.
type core struct {
	bot *luna.Bot
	cfg Settings

	mu       sync.Mutex
	live     bool
	game     string
	messages int64 // chat messages seen, for timers' "only when chat is active"
	essentials
	moderation
	events
	shoutouts
}

// New builds the bundle's bot.
func New() *luna.Bot {
	bot := luna.New()
	c := &core{bot: bot}
	bot.UseConfig(&c.cfg)
	bot.OnStart(func(ctx *luna.Ctx) error {
		if s, err := bot.Twitch.Stream(ctx, ""); err == nil {
			c.mu.Lock()
			c.live, c.game = s.Live, s.Game
			c.mu.Unlock()
		}
		if ch, err := bot.Twitch.Channel(ctx, ""); err == nil && ch != nil {
			c.mu.Lock()
			c.game = ch.Game
			c.mu.Unlock()
		}
		return nil
	})
	bot.On("twitch.stream.online", func(ctx *luna.Ctx, ev *luna.Event) error {
		c.mu.Lock()
		c.live = true
		c.friendsSeen = map[string]bool{}
		c.mu.Unlock()
		return nil
	})
	bot.On("twitch.stream.offline", func(ctx *luna.Ctx, ev *luna.Event) error {
		c.mu.Lock()
		c.live = false
		c.mu.Unlock()
		return nil
	})
	bot.On("twitch.channel.update", func(ctx *luna.Ctx, ev *luna.Event) error {
		c.mu.Lock()
		c.game = ev.Str("game")
		c.mu.Unlock()
		return nil
	})
	// One handler sees every chat message, in order: moderation first, so a message that
	// gets deleted doesn't also trigger a custom command or a welcome.
	bot.On("twitch.chat.message", func(ctx *luna.Ctx, ev *luna.Event) error {
		c.mu.Lock()
		c.messages++
		c.mu.Unlock()
		if removed, err := c.moderate(ctx, ev); removed || err != nil {
			return err
		}
		if err := c.welcome(ctx, ev); err != nil {
			return err
		}
		if err := c.autoShoutout(ctx, ev); err != nil {
			return err
		}
		return c.chatExtras(ctx, ev)
	})
	c.setupEssentials()
	c.setupModeration()
	c.setupEvents()
	c.setupShoutouts()
	return bot
}

// Main runs the bundle as a capsule: `luna __bundle` inside the sandbox.
func Main() { New().Run() }

func (c *core) isLive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live
}

func (c *core) currentGame() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.game
}

// ---------- timeline: every change to viewers' state, with undo ----------

type entry struct {
	ID     int64     `json:"id"`
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"` // login, or "luna" for automatic actions
	Rank   string    `json:"rank"`
	Action string    `json:"action"` // timeout, ban, delete, warn, shield, flag, nuke, counter
	User   string    `json:"user,omitempty"`
	UserID string    `json:"user_id,omitempty"`
	Detail string    `json:"detail,omitempty"`
	Undo   *undo     `json:"undo,omitempty"` // nil: can't be undone
	Undone bool      `json:"undone,omitempty"`
}

// undo is what reverses an entry: lift timeouts and bans, end Shield Mode, or put a counter back.
type undo struct {
	Unban   []string `json:"unban,omitempty"`
	ShieldO bool     `json:"shield_off,omitempty"`
	Counter string   `json:"counter,omitempty"`
	Value   int64    `json:"value,omitempty"`
}

const timelineKeep = 500

func tlKey(id int64) string { return fmt.Sprintf("tl:%012d", id) }

func actorOf(ctx *luna.Ctx) (string, string) {
	if u := ctx.User(); u != nil && ctx.Event != nil && ctx.Event.Type == "twitch.chat.message" {
		rank := u.Rank
		if rank == "" {
			rank = "viewer"
		}
		return u.Login, rank
	}
	return "luna", "luna"
}

func (c *core) record(ctx *luna.Ctx, e entry) (int64, error) {
	id, err := c.bot.Store.Incr(ctx, "tl.seq", 1)
	if err != nil {
		return 0, err
	}
	e.ID, e.At = id, c.bot.Now()
	if e.Actor == "" {
		e.Actor, e.Rank = actorOf(ctx)
	}
	var b luna.Batch
	b.Set(tlKey(id), e, 0)
	if id > timelineKeep {
		b.Delete(tlKey(id - timelineKeep))
	}
	return id, c.bot.Store.Apply(ctx, &b)
}

func (c *core) timeline(ctx *luna.Ctx, n int) ([]entry, error) {
	keys, err := c.bot.Store.Keys(ctx, "tl:")
	if err != nil {
		return nil, err
	}
	slices.Reverse(keys)
	var out []entry
	for _, k := range keys[:min(n, len(keys))] {
		var e entry
		if ok, err := c.bot.Store.Get(ctx, k, &e); err != nil {
			return nil, err
		} else if ok {
			out = append(out, e)
		}
	}
	return out, nil
}

var rankOrder = []string{"luna", "viewer", "moderator", "luna-mod", "head-mod", "broadcaster"}

// undoEntry reverses entry id as actor. Like Luna's override rules, someone may undo their
// own actions, Luna's automatic ones, and those of lower ranks.
func (c *core) undoEntry(ctx *luna.Ctx, id int64, actor, rank string) (string, error) {
	var e entry
	if ok, err := c.bot.Store.Get(ctx, tlKey(id), &e); err != nil {
		return "", err
	} else if !ok {
		return "", fmt.Errorf("no timeline entry #%d", id)
	}
	switch {
	case e.Undone:
		return "", fmt.Errorf("#%d is already undone", id)
	case e.Undo == nil:
		return "", fmt.Errorf("#%d (%s) can't be undone", id, e.Action)
	case rank != "broadcaster" && actor != e.Actor && slices.Index(rankOrder, rank) <= slices.Index(rankOrder, e.Rank):
		return "", fmt.Errorf("#%d was made by a %s", id, e.Rank)
	}
	for _, uid := range e.Undo.Unban {
		if err := c.bot.Mod.Unban(ctx, uid); err != nil {
			return "", err
		}
	}
	if e.Undo.ShieldO {
		if err := c.setShield(ctx, false, ""); err != nil {
			return "", err
		}
	}
	if e.Undo.Counter != "" {
		if err := c.bot.Store.Set(ctx, e.Undo.Counter, e.Undo.Value, 0); err != nil {
			return "", err
		}
	}
	e.Undone = true
	if err := c.bot.Store.Set(ctx, tlKey(id), e, 0); err != nil {
		return "", err
	}
	return fmt.Sprintf("undid #%d (%s)", id, strings.TrimSpace(e.Action+" "+e.User)), nil
}

// ---------- templates ----------

var varRe = regexp.MustCompile(`\{([a-z]+)(?: ([^}]*))?\}`)

// render fills {name} variables from vars; {random 1-100} and {pick a|b|c} work anywhere.
// Unknown variables stay as written, so a typo is visible rather than silently blank.
func render(tmpl string, vars map[string]string) string {
	return varRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		p := varRe.FindStringSubmatch(m)
		switch p[1] {
		case "random":
			lo, hi, ok := strings.Cut(p[2], "-")
			a, err1 := strconv.Atoi(strings.TrimSpace(lo))
			b, err2 := strconv.Atoi(strings.TrimSpace(hi))
			if !ok || err1 != nil || err2 != nil || b < a {
				return m
			}
			return strconv.Itoa(a + rand.IntN(b-a+1))
		case "pick":
			opts := strings.Split(p[2], "|")
			return strings.TrimSpace(opts[rand.IntN(len(opts))])
		}
		if v, ok := vars[p[1]]; ok && p[2] == "" {
			return v
		}
		return m
	})
}

// human is a duration the way chat says it: "3 days 4 hours", "12 minutes".
func human(d time.Duration) string {
	if d < time.Minute {
		return plural(int64(d/time.Second), "second")
	}
	units := []struct {
		d    time.Duration
		name string
	}{{365 * 24 * time.Hour, "year"}, {30 * 24 * time.Hour, "month"}, {24 * time.Hour, "day"}, {time.Hour, "hour"}, {time.Minute, "minute"}}
	var parts []string
	for _, u := range units {
		if n := int64(d / u.d); n > 0 && len(parts) < 2 {
			parts = append(parts, plural(n, u.name))
			d -= time.Duration(n) * u.d
		}
	}
	return strings.Join(parts, " ")
}

func plural(n int64, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// login normalizes "@Fox" to "fox".
func login(s string) string { return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "@")) }

// ---------- folding, so lookalike letters and l33t don't slip past filters ----------

var confusables = map[rune]rune{
	'а': 'a', 'е': 'e', 'о': 'o', 'р': 'p', 'с': 'c', 'у': 'y', 'х': 'x', 'і': 'i', 'ј': 'j', 'ѕ': 's',
	'ԁ': 'd', 'ӏ': 'l', 'ɡ': 'g', 'κ': 'k', 'ν': 'v', 'ο': 'o', 'ρ': 'p', 'τ': 't', 'υ': 'u', 'χ': 'x',
	'0': 'o', '1': 'i', '3': 'e', '4': 'a', '5': 's', '7': 't', '@': 'a', '$': 's', '!': 'i', '|': 'l',
}

// fold maps text to a plain lowercase form: compatibility forms and accents dropped,
// lookalike letters and digits mapped to the Latin letter they imitate, zero-width gone.
func fold(s string) string {
	var b strings.Builder
	for _, r := range norm.NFKD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Cf, r):
			continue
		}
		r = unicode.ToLower(r)
		if m, ok := confusables[r]; ok {
			r = m
		}
		b.WriteRune(r)
	}
	return b.String()
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
