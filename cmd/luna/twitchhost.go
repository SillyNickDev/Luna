package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"luna/internal/event"
	"luna/internal/guard"
	"luna/internal/plugin/pb"
	"luna/internal/store"
	"luna/internal/twitch"
)

// twitchHost is what plugins reach of Twitch: moderation as the broadcaster, chat as
// the chatter (the bot account if there is one), and public lookups.
type twitchHost struct {
	bc, chatter *twitch.Client
	echo        echoes
}

// echoes remembers what Luna just said, so its own messages coming back through EventSub
// don't run commands. Recorded before sending, so the echo can't beat it.
type echoes struct {
	mu   sync.Mutex
	sent map[string]time.Time // normalized text → when Luna sent it
}

func normalize(s string) string { return strings.Join(strings.Fields(s), " ") }

func (e *echoes) add(text string, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sent == nil {
		e.sent = map[string]time.Time{}
	}
	for k, at := range e.sent {
		if now.Sub(at) > 30*time.Second {
			delete(e.sent, k)
		}
	}
	e.sent[normalize(text)] = now
}

// take reports whether Luna sent text in the last 30 s, forgetting it so a repeat by a person counts.
func (e *echoes) take(text string, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	k := normalize(text)
	at, ok := e.sent[k]
	delete(e.sent, k)
	return ok && now.Sub(at) <= 30*time.Second
}

// own reports whether a chat message is one Luna sent. With a bot account that's anything
// the bot says; chatting as the broadcaster, only what Luna just sent, so the broadcaster's
// own !commands still work.
func (t *twitchHost) own(userID, text string) bool {
	if userID != t.chatter.UserID() {
		return false
	}
	return t.chatter != t.bc || t.echo.take(text, time.Now())
}

func (t *twitchHost) send(ctx context.Context, m guard.Msg) error {
	if m.Mode != "whisper" {
		t.echo.add(m.Text, time.Now())
	}
	b := t.bc.UserID()
	switch {
	case m.Mode == "announce":
		return t.bc.Announce(ctx, m.Text)
	case m.Mode == "whisper":
		return t.chatter.Whisper(ctx, m.To, m.Text)
	case m.ReplyTo != "":
		return t.chatter.SendReply(ctx, b, m.Text, m.ReplyTo)
	}
	return t.chatter.SendChat(ctx, b, m.Text)
}

func (t *twitchHost) Mod(ctx context.Context, r *pb.ModRequest) error {
	switch r.Action {
	case "timeout", "ban":
		return t.bc.Ban(ctx, r.UserId, r.Seconds, r.Reason)
	case "unban":
		return t.bc.Unban(ctx, r.UserId)
	case "delete":
		return t.bc.DeleteMessage(ctx, r.MessageId)
	case "warn":
		return t.bc.Warn(ctx, r.UserId, r.Reason)
	case "shield":
		return t.bc.Shield(ctx, r.On)
	case "shoutout":
		return t.bc.Shoutout(ctx, r.UserId)
	}
	return fmt.Errorf("unknown mod action %q", r.Action)
}

func (t *twitchHost) Lookup(ctx context.Context, call, user string, raw []byte) (any, error) {
	if user == "" {
		user = t.bc.UserID()
	}
	var args struct {
		Query       string   `json:"query"`
		Logins      []string `json:"logins"`
		Title       string   `json:"title"`
		GameID      string   `json:"game_id"`
		Description string   `json:"description"`
		ToID        string   `json:"to_id"`
		ID          string   `json:"id"`
		Choices     []string `json:"choices"`
		DurationS   int      `json:"duration_s"`
		Status      string   `json:"status"`
		Winner      string   `json:"winner"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("%s: %w", call, err)
		}
	}
	switch call {
	case "ban": // internal: plugins can't ask for it (it isn't in the plugin server's twitchCalls)
		banned, until, err := t.bc.Banned(ctx, user)
		if err != nil || !banned {
			return nil, err
		}
		return map[string]any{"permanent": until.IsZero(), "until": until}, nil
	case "poll":
		return t.bc.StartPoll(ctx, args.Title, args.Choices, args.DurationS)
	case "end_poll":
		return t.bc.EndPoll(ctx, args.ID)
	case "prediction":
		return t.bc.StartPrediction(ctx, args.Title, args.Choices, args.DurationS)
	case "end_prediction":
		return t.bc.EndPrediction(ctx, args.ID, args.Status, args.Winner)
	case "categories":
		return t.bc.SearchCategories(ctx, args.Query)
	case "schedule":
		return t.bc.Schedule(ctx)
	case "streams":
		return t.bc.Streams(ctx, args.Logins)
	case "set_channel":
		if args.Title == "" && args.GameID == "" {
			return nil, errors.New("set_channel needs a title or game_id")
		}
		return nil, t.bc.SetChannel(ctx, args.Title, args.GameID)
	case "clip":
		return t.bc.Clip(ctx)
	case "marker":
		return t.bc.Marker(ctx, args.Description)
	case "ads":
		return t.bc.Ads(ctx)
	case "snooze":
		return t.bc.SnoozeAd(ctx)
	case "raid":
		if !isID(args.ToID) {
			return nil, errors.New("raid needs to_id, a channel's user ID")
		}
		return nil, t.bc.Raid(ctx, args.ToID)
	case "unraid":
		return nil, t.bc.Unraid(ctx)
	case "user":
		u, err := t.bc.LookupUser(ctx, strings.ToLower(strings.TrimPrefix(user, "@")), isID(user))
		if u == nil {
			return nil, err
		}
		return u, err
	case "channel":
		c, err := t.bc.Channel(ctx, user)
		if c == nil {
			return nil, err
		}
		return c, err
	case "stream":
		return t.bc.Stream(ctx, user)
	case "follow":
		at, err := t.bc.FollowedAt(ctx, user)
		if err != nil || at.IsZero() {
			return nil, err
		}
		return map[string]time.Time{"followed_at": at}, nil
	}
	return nil, fmt.Errorf("unknown twitch call %q", call)
}

func isID(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// viewerTracker numbers live streams and stamps each chat message with whether it's the
// sender's first ever ("first") and how long they'd been away ("away_s"), from the
// viewers table. Simulated chat only gets "first", from memory, so it never touches stats.
type viewerTracker struct {
	st *store.Store

	mu      sync.Mutex
	stream  int64 // current stream's number, 0 while offline
	simSeen map[string]bool
}

// online records a stream start; a restart mid-stream keeps the same number.
func (vt *viewerTracker) online(startedAt string) {
	vt.mu.Lock()
	defer vt.mu.Unlock()
	if t, err := time.Parse(time.RFC3339Nano, startedAt); err == nil {
		startedAt = t.UTC().Truncate(time.Second).Format(time.RFC3339) // EventSub has milliseconds, Helix doesn't
	}
	prev, _ := vt.st.PluginGet("luna", "stream.started")
	cur, _ := json.Marshal(startedAt)
	if string(prev) != string(cur) {
		vt.st.PluginIncr("luna", "stream.seq", 1)
		vt.st.PluginSet("luna", "stream.started", cur, 0)
	}
	seq, _ := vt.st.PluginGet("luna", "stream.seq")
	json.Unmarshal(seq, &vt.stream)
}

func (vt *viewerTracker) wrap(publish func(event.Event)) func(event.Event) {
	return func(ev event.Event) {
		switch {
		case ev.Type == "twitch.stream.online" && !ev.Simulated:
			var d struct {
				StartedAt string `json:"started_at"`
			}
			json.Unmarshal(ev.Data, &d)
			vt.online(d.StartedAt)
		case ev.Type == "twitch.stream.offline" && !ev.Simulated:
			vt.mu.Lock()
			vt.stream = 0
			vt.mu.Unlock()
		case ev.Type == "twitch.chat.message":
			ev = vt.stamp(ev)
		}
		publish(ev)
	}
}

func (vt *viewerTracker) stamp(ev event.Event) event.Event {
	var m map[string]json.RawMessage
	var u struct {
		ID    string `json:"id"`
		Login string `json:"login"`
	}
	if json.Unmarshal(ev.Data, &m) != nil || json.Unmarshal(m["user"], &u) != nil || u.ID == "" {
		return ev
	}
	first, away := false, int64(0)
	vt.mu.Lock()
	if ev.Simulated {
		if vt.simSeen == nil || len(vt.simSeen) > 10000 {
			vt.simSeen = map[string]bool{}
		}
		first = !vt.simSeen[u.ID]
		vt.simSeen[u.ID] = true
	} else if prev, err := vt.st.SeeChatter(u.ID, u.Login, ev.TS, vt.stream); err != nil {
		slog.Error("viewer stats write failed", "err", err)
	} else {
		first = prev == nil
		if prev != nil {
			away = int64(ev.TS.Sub(prev.LastSeen).Seconds())
		}
	}
	vt.mu.Unlock()
	m["first"], _ = json.Marshal(first)
	m["away_s"], _ = json.Marshal(away)
	ev.Data, _ = json.Marshal(m)
	return ev
}
