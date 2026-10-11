// Package guard is the output guard: every outbound chat message passes through it,
// whichever plugin asked, so the blocklist and rate limit always apply.
package guard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"luna/internal/config"
)

var ErrBlocked = errors.New("blocked by the output guard")

const maxLen = 500 // Twitch's chat message limit

// Msg is one outbound chat message.
type Msg struct {
	Text      string
	ReplyTo   string // message_id it answers
	Mode      string // "", "announce" or "whisper"
	To        string // whisper recipient's user id
	Simulated bool
}

type Guard struct {
	cfg  func() *config.Config
	Live func(ctx context.Context, m Msg) error // Twitch; nil until connected
	Sim  func(ctx context.Context, m Msg) error // simulated events reply here

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func New(cfg func() *config.Config) *Guard {
	return &Guard{cfg: cfg, Sim: func(_ context.Context, m Msg) error {
		switch m.Mode {
		case "whisper":
			fmt.Printf("[sim whisper to %s] %s\n", m.To, m.Text)
		case "announce":
			fmt.Println("[sim announce]", m.Text)
		default:
			fmt.Println("[sim chat]", m.Text)
		}
		return nil
	}}
}

// Chat sends text after cleaning it and checking the blocklist. Live messages
// wait for the rate limiter, so bursts queue until ctx expires instead of failing.
func (g *Guard) Chat(ctx context.Context, text string, simulated bool) error {
	return g.Send(ctx, Msg{Text: text, Simulated: simulated})
}

// Blocked reports whether text matches the chat.blocklist; the VRChat chatbox uses it too.
func (g *Guard) Blocked(text string) bool {
	for _, re := range g.cfg().Chat.Patterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// Send is Chat for any kind of message: a reply, an announcement or a whisper.
func (g *Guard) Send(ctx context.Context, m Msg) error {
	c := g.cfg()
	m.Text = Clean(m.Text)
	text := m.Text
	if text == "" {
		return errors.New("empty message")
	}
	if g.Blocked(text) {
		return ErrBlocked
	}
	if m.Simulated {
		return g.Sim(ctx, m)
	}
	if g.Live == nil {
		return ErrOffline
	}
	if err := g.wait(ctx, c.Chat.RateLimit); err != nil {
		return err
	}
	return g.Live(ctx, m)
}

var ErrOffline = errors.New("twitch is not connected")

// Mod runs a moderation action, or prints desc instead when it answers a simulated event.
// shortcut: no rate limit on mod actions; Twitch's own (~100/min) applies, add one if nukes hit it.
func (g *Guard) Mod(ctx context.Context, desc string, simulated bool, act func(context.Context) error) error {
	if simulated {
		fmt.Println("[sim mod]", desc)
		return nil
	}
	return act(ctx)
}

// Clean flattens control characters, escapes a leading / or . so relayed viewer
// text can't become a chat command, and truncates to Twitch's limit.
// ponytail: no confusables folding for the blocklist yet; add with the Moderation filters.
func Clean(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") {
		s = "​" + s // zero-width space
	}
	if r := []rune(s); len(r) > maxLen {
		s = string(r[:maxLen])
	}
	return s
}

// wait is a token bucket allowing per30s messages every 30 seconds.
func (g *Guard) wait(ctx context.Context, per30s int) error {
	max := float64(per30s)
	rate := max / 30
	for {
		g.mu.Lock()
		now := time.Now()
		if g.last.IsZero() {
			g.tokens = max
		} else {
			g.tokens = min(max, g.tokens+now.Sub(g.last).Seconds()*rate)
		}
		g.last = now
		if g.tokens >= 1 {
			g.tokens--
			g.mu.Unlock()
			return nil
		}
		d := time.Duration((1 - g.tokens) / rate * float64(time.Second))
		g.mu.Unlock()
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Headroom is how many messages could go out right now before the rate limit holds them.
func (g *Guard) Headroom() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	max := float64(g.cfg().Chat.RateLimit)
	if g.last.IsZero() {
		return max
	}
	return min(max, g.tokens+time.Since(g.last).Seconds()*max/30)
}
