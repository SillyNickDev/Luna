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

type Guard struct {
	cfg  func() *config.Config
	Live func(ctx context.Context, text string) error // Twitch; nil until connected
	Sim  func(ctx context.Context, text string) error // simulated events reply here

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func New(cfg func() *config.Config) *Guard {
	return &Guard{cfg: cfg, Sim: func(_ context.Context, text string) error {
		fmt.Println("[sim chat]", text)
		return nil
	}}
}

// Chat sends text after cleaning it and checking the blocklist. Live messages
// wait for the rate limiter, so bursts queue until ctx expires instead of failing.
func (g *Guard) Chat(ctx context.Context, text string, simulated bool) error {
	c := g.cfg()
	text = Clean(text)
	if text == "" {
		return errors.New("empty message")
	}
	for _, re := range c.Chat.Patterns {
		if re.MatchString(text) {
			return ErrBlocked
		}
	}
	if simulated {
		return g.Sim(ctx, text)
	}
	if g.Live == nil {
		return errors.New("twitch is not connected")
	}
	if err := g.wait(ctx, c.Chat.RateLimit); err != nil {
		return err
	}
	return g.Live(ctx, text)
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
