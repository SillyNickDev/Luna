package guard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"luna/internal/config"
)

func TestGuard(t *testing.T) {
	c := config.Default()
	c.Chat.RateLimit = 2
	c.Chat.Blocklist = []string{`bad\s*word`}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	g := New(func() *config.Config { return c })
	var sent []string
	g.Live = func(_ context.Context, s string) error { sent = append(sent, s); return nil }
	ctx := context.Background()

	if err := g.Chat(ctx, "BAD  WORD here", false); !errors.Is(err, ErrBlocked) {
		t.Fatalf("blocklist: err = %v", err)
	}
	if err := g.Chat(ctx, "  /ban someone\n", false); err != nil {
		t.Fatal(err)
	}
	if sent[0] != "​/ban someone" {
		t.Fatalf("command not escaped: %q", sent[0])
	}
	if got := Clean(strings.Repeat("a", 600)); len(got) != maxLen {
		t.Fatalf("not truncated: %d", len(got))
	}

	g.Chat(ctx, "two", false) // uses the last token
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := g.Chat(short, "three", false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("rate limit: err = %v", err)
	}
}
