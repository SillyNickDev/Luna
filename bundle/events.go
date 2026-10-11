package bundle

import (
	"slices"
	"strconv"
	"strings"
	"time"

	luna "luna/sdk/go"
)

type events struct {
	followers   []string // names waiting for the batched welcome
	followTimer bool
}

func (c *core) on(kind string) bool { return slices.Contains(csv(c.cfg.EventsOn), kind) }

// joinNames reads "Ana", "Ana and Ben", "Ana, Ben and Cy", or "Ana, Ben, Cy and 4 others".
func joinNames(names []string) string {
	switch n := len(names); {
	case n == 1:
		return names[0]
	case n <= 3:
		return strings.Join(names[:n-1], ", ") + " and " + names[n-1]
	default:
		return strings.Join(names[:3], ", ") + " and " + plural(int64(n-3), "other")
	}
}

func (c *core) setupEvents() {
	bot := c.bot
	// tmpl points into c.cfg, which is only loaded once the bot connects.
	thank := func(kind string, tmpl *string, vars func(*luna.Event) map[string]string) func(*luna.Ctx, *luna.Event) error {
		return func(ctx *luna.Ctx, ev *luna.Event) error {
			if !c.on(kind) {
				return nil
			}
			v := vars(ev)
			v["user"] = ev.User().DisplayName()
			return bot.Chat.Send(ctx, render(*tmpl, v))
		}
	}
	none := func(*luna.Event) map[string]string { return map[string]string{} }

	// Follows are batched: the first starts a short window, and one message welcomes everyone in it.
	bot.On("twitch.channel.follow", func(ctx *luna.Ctx, ev *luna.Event) error {
		if !c.on("follow") {
			return nil
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.followers) < 1000 {
			c.followers = append(c.followers, ev.User().DisplayName())
		}
		if !c.followTimer {
			c.followTimer = true
			bot.Timers.After(ctx, time.Duration(max(c.cfg.FollowBatchS, 1))*time.Second, func(ctx *luna.Ctx) error {
				c.mu.Lock()
				names := c.followers
				c.followers, c.followTimer = nil, false
				c.mu.Unlock()
				return bot.Chat.Send(ctx, render(c.cfg.FollowMessage, map[string]string{"names": joinNames(names)}))
			})
		}
		return nil
	})
	// Each gifted sub also arrives as a subscribe with gift=true; the gift event thanks
	// the gifter once, so those stay quiet instead of 50 messages for a gift bomb.
	bot.On("twitch.channel.subscribe", thank("sub", &c.cfg.SubMessage, none), luna.When(func(ev *luna.Event) bool { return !ev.Bool("gift") }))
	bot.On("twitch.channel.resub", thank("resub", &c.cfg.ResubMessage, func(ev *luna.Event) map[string]string {
		return map[string]string{"months": strconv.FormatInt(ev.Int("months"), 10)}
	}))
	bot.On("twitch.channel.gift", func(ctx *luna.Ctx, ev *luna.Event) error {
		if !c.on("gift") {
			return nil
		}
		name := ev.User().DisplayName()
		if ev.Bool("anonymous") || name == "" {
			name = "An anonymous gifter"
		}
		return bot.Chat.Send(ctx, render(c.cfg.GiftMessage, map[string]string{"user": name, "count": strconv.FormatInt(ev.Int("count"), 10)}))
	})
	bot.On("twitch.channel.cheer", func(ctx *luna.Ctx, ev *luna.Event) error {
		if !c.on("bits") {
			return nil
		}
		name := ev.User().DisplayName()
		if ev.Bool("anonymous") || name == "" {
			name = "Someone"
		}
		return bot.Chat.Send(ctx, render(c.cfg.BitsMessage, map[string]string{"user": name, "bits": strconv.FormatInt(ev.Int("bits"), 10)}))
	})
	bot.On("twitch.channel.raid", thank("raid", &c.cfg.RaidMessage, func(ev *luna.Event) map[string]string {
		return map[string]string{"viewers": strconv.FormatInt(ev.Int("viewers"), 10)}
	}))
}

// welcome greets first-time chatters, and regulars back after welcome_back_days away.
func (c *core) welcome(ctx *luna.Ctx, ev *luna.Event) error {
	u := ev.User()
	switch {
	case ev.Bool("first") && c.on("first"):
		return c.bot.Chat.Send(ctx, render(c.cfg.FirstMessage, map[string]string{"user": u.DisplayName()}))
	case c.on("welcome_back") && ev.Int("away_s") >= int64(c.cfg.WelcomeBackDays)*86400:
		if ok, err := c.isRegular(ctx, u.ID); err != nil || !ok {
			return err
		}
		return c.bot.Chat.Send(ctx, render(c.cfg.WelcomeBackMessage, map[string]string{"user": u.DisplayName()}))
	}
	return nil
}
