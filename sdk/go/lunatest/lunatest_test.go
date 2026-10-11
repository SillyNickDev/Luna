package lunatest

import (
	"errors"
	"testing"
	"time"

	luna "luna/sdk/go"
)

func helloBot() *luna.Bot {
	bot := luna.New()
	var cfg struct {
		Greeting string `luna:"greeting"`
	}
	cfg.Greeting = "Hello"
	bot.UseConfig(&cfg)
	bot.On("twitch.channel.follow", func(c *luna.Ctx, ev *luna.Event) error {
		if _, err := bot.Store.Incr(c, "follows", 1); err != nil {
			return err
		}
		return bot.Chat.Send(c, cfg.Greeting+", "+ev.User().DisplayName()+"!")
	})
	bot.ChatCommand(luna.ChatCommand{Name: "hi", Cooldown: "10s"}, func(c *luna.Ctx, args []string) error {
		return c.Reply("hi " + c.User().Login)
	})
	bot.ChatCommand(luna.ChatCommand{Name: "zap", Capability: "mod.timeout"}, func(c *luna.Ctx, args []string) error {
		return bot.Mod.Timeout(c, "id-"+args[0], time.Minute, "zapped")
	})
	bot.CLI(luna.CLICommand{Name: "count"}, func(c *luna.Ctx, a luna.Args) (any, error) {
		var n int
		_, err := bot.Store.Get(c, "follows", &n)
		return map[string]int{"follows": n}, err
	})
	bot.Timers.Every(time.Minute, func(c *luna.Ctx) error { return bot.Chat.Send(c, "tick") })
	return bot
}

func TestHarness(t *testing.T) {
	h := New(t, helloBot(), "testdata", map[string]any{"greeting": "Welcome to the pack"})
	h.Emit("twitch.channel.follow", map[string]any{"user": "fox"})
	if got := h.Said(); len(got) != 1 || got[0] != "Welcome to the pack, fox!" {
		t.Fatalf("follow: %q", got)
	}
	if got := h.Chat("fox", "!hi"); len(got) != 1 || got[0] != "hi fox" {
		t.Fatalf("!hi: %q", got)
	}
	if got := h.Chat("fox", "!hi"); len(got) != 0 {
		t.Fatalf("!hi on cooldown answered: %q", got)
	}
	if h.Messages()[1].ReplyTo == "" {
		t.Fatal("ctx.Reply didn't thread the reply")
	}
	h.Chat("fox", "!zap wolf") // a viewer lacks mod.timeout: Luna never routes it
	h.Chat("owl", "!zap wolf", "moderator")
	if got := h.Twitch.Mods(); len(got) != 1 || got[0] != "timeout id-wolf 60s" {
		t.Fatalf("mods: %q", got)
	}
	if v, err := h.CLI("count", nil); err != nil || v.(map[string]any)["follows"] != 1.0 {
		t.Fatalf("cli: %v %v", v, err)
	}
	h.Clock.Advance(150 * time.Second)
	if got := h.Said(); len(got) != 2 {
		t.Fatalf("two minutes of a one-minute timer: %q", got)
	}
}

func TestPermissionsEnforced(t *testing.T) {
	bot := luna.New()
	var err error
	bot.On("twitch.channel.follow", func(c *luna.Ctx, ev *luna.Event) error {
		err = bot.Mod.Ban(c, "x", "")
		return nil
	})
	h := New(t, bot, "testdata", nil)
	h.Emit("twitch.channel.follow", map[string]any{"user": "fox"})
	if !errors.Is(err, luna.ErrPermissionDenied) {
		t.Fatalf("ban without [permissions].mod ban: %v", err)
	}
}
