package bundle

import (
	"slices"
	"time"

	luna "luna/sdk/go"
)

// Twitch's native shoutout cooldowns.
const (
	soGlobalCooldown  = 2 * time.Minute
	soChannelCooldown = time.Hour
)

type shoutouts struct {
	friendsSeen map[string]bool // friends already shouted out this stream
	soQueue     []string        // channel ids waiting for the native cooldown
	soLast      time.Time
	soTo        map[string]time.Time
}

func (c *core) setupShoutouts() {
	bot := c.bot
	c.soTo, c.friendsSeen = map[string]time.Time{}, map[string]bool{}
	c.chat(luna.ChatCommand{Name: "so", Aliases: []string{"shoutout"}, Capability: "stream.shoutout"}, func(ctx *luna.Ctx, args []string) error {
		if len(args) != 1 {
			return ctx.Reply("Usage: !so <user>")
		}
		return c.shoutout(ctx, login(args[0]))
	})
	bot.On("twitch.channel.raid", func(ctx *luna.Ctx, ev *luna.Event) error {
		if !c.cfg.SoRaids {
			return nil
		}
		return c.shoutout(ctx, ev.User().Login)
	})
	bot.Timers.Every(30*time.Second, c.drainShoutouts)
}

// shoutout posts who to check out, and sends Twitch's native shoutout now or, if it's on
// cooldown, as soon as it's off.
func (c *core) shoutout(ctx *luna.Ctx, who string) error {
	u, err := c.bot.Twitch.User(ctx, who)
	if err != nil {
		return err
	}
	if u == nil {
		return c.bot.Chat.Send(ctx, "No Twitch user "+who+".")
	}
	ch, err := c.bot.Twitch.Channel(ctx, u.ID)
	if err != nil {
		return err
	}
	vars := map[string]string{"name": u.Name, "login": u.Login, "game": "something", "title": ""}
	if ch != nil && ch.Game != "" {
		vars["game"], vars["title"] = ch.Game, ch.Title
	}
	if err := c.bot.Chat.Send(ctx, render(c.cfg.SoMessage, vars)); err != nil {
		return err
	}
	if !c.cfg.SoNative {
		return nil
	}
	c.mu.Lock()
	if !slices.Contains(c.soQueue, u.ID) && len(c.soQueue) < 20 {
		c.soQueue = append(c.soQueue, u.ID)
	}
	c.mu.Unlock()
	return c.drainShoutouts(ctx)
}

// drainShoutouts sends the next queued native shoutout once Twitch's cooldowns allow.
func (c *core) drainShoutouts(ctx *luna.Ctx) error {
	now := c.bot.Now()
	c.mu.Lock()
	if len(c.soQueue) == 0 || now.Sub(c.soLast) < soGlobalCooldown {
		c.mu.Unlock()
		return nil
	}
	i := slices.IndexFunc(c.soQueue, func(id string) bool { return now.Sub(c.soTo[id]) >= soChannelCooldown })
	if i < 0 {
		c.mu.Unlock()
		return nil
	}
	id := c.soQueue[i]
	c.soQueue = slices.Delete(c.soQueue, i, i+1)
	c.soLast, c.soTo[id] = now, now
	c.mu.Unlock()
	if !c.isLive() {
		return nil // Twitch only allows shoutouts while live
	}
	return c.bot.Mod.Shoutout(ctx, id)
}

// autoShoutout shouts out a friend (so_friends) the first time they chat each stream.
func (c *core) autoShoutout(ctx *luna.Ctx, ev *luna.Event) error {
	u := ev.User()
	if u == nil || !c.isLive() || !slices.Contains(csv(c.cfg.SoFriends), u.Login) {
		return nil
	}
	c.mu.Lock()
	seen := c.friendsSeen[u.Login]
	c.friendsSeen[u.Login] = true
	c.mu.Unlock()
	if seen {
		return nil
	}
	return c.shoutout(ctx, u.Login)
}
