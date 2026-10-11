package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"luna/internal/event"
)

const wsURL = "wss://eventsub.wss.twitch.tv/ws"

type wsMsg struct {
	Metadata struct {
		MessageID        string    `json:"message_id"`
		MessageType      string    `json:"message_type"`
		MessageTimestamp time.Time `json:"message_timestamp"`
		SubscriptionType string    `json:"subscription_type"`
	} `json:"metadata"`
	Payload struct {
		Session struct {
			ID                      string `json:"id"`
			KeepaliveTimeoutSeconds int    `json:"keepalive_timeout_seconds"`
			ReconnectURL            string `json:"reconnect_url"`
		} `json:"session"`
		Subscription struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"subscription"`
		Event json.RawMessage `json:"event"`
	} `json:"payload"`
}

type session struct {
	*websocket.Conn
	keepalive time.Duration
}

// Sub is one EventSub subscription.
type Sub struct {
	Type, Version string
	Condition     map[string]string
}

// ChannelSubs are the broadcaster's channel events, made with the broadcaster's token.
func ChannelSubs(b string) []Sub {
	return []Sub{
		{"channel.follow", "2", map[string]string{"broadcaster_user_id": b, "moderator_user_id": b}},
		{"channel.subscribe", "1", map[string]string{"broadcaster_user_id": b}},
		{"channel.cheer", "1", map[string]string{"broadcaster_user_id": b}},
		{"channel.raid", "1", map[string]string{"to_broadcaster_user_id": b}},
		{"channel.channel_points_custom_reward_redemption.add", "1", map[string]string{"broadcaster_user_id": b}},
		{"channel.moderator.add", "1", map[string]string{"broadcaster_user_id": b}},
		{"channel.moderator.remove", "1", map[string]string{"broadcaster_user_id": b}},
		{"stream.online", "1", map[string]string{"broadcaster_user_id": b}},
		{"stream.offline", "1", map[string]string{"broadcaster_user_id": b}},
		{"channel.update", "2", map[string]string{"broadcaster_user_id": b}},
		{"channel.subscription.gift", "1", map[string]string{"broadcaster_user_id": b}},
		{"channel.subscription.message", "1", map[string]string{"broadcaster_user_id": b}},
		{"channel.ad_break.begin", "1", map[string]string{"broadcaster_user_id": b}},
	}
}

// ChatSub reads broadcaster b's chat as reader, made with the reader's token.
func ChatSub(b, reader string) Sub {
	return Sub{"channel.chat.message", "1", map[string]string{"broadcaster_user_id": b, "user_id": reader}}
}

// Run keeps one EventSub WebSocket open for subs and publishes events until ctx ends.
// Each account gets its own connection, since every subscription on a session must
// be made with that account's token. It returns early only with ErrAuth.
func (c *Client) Run(ctx context.Context, subs []Sub, publish func(event.Event)) error {
	go c.validateHourly(ctx)
	seen := newDedupe(1024)
	backoff := time.Second
	var s *session // set when a reconnect already opened the next connection
	for {
		var err error
		if s == nil {
			s, err = c.connect(ctx, wsURL, subs)
		}
		if err == nil {
			slog.Info("twitch connected", "account", c.account, "user_id", c.userID)
			started := time.Now()
			s, err = c.read(ctx, s, seen, publish)
			if s != nil {
				continue
			}
			if time.Since(started) > time.Minute {
				backoff = time.Second
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrAuth) {
			return err
		}
		slog.Warn("twitch disconnected, retrying", "account", c.account, "err", err, "in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil
		}
		backoff = min(backoff*2, 2*time.Minute)
	}
}

// connect dials, waits for the welcome, and creates subs (none after a reconnect, where they carry over).
func (c *Client) connect(ctx context.Context, u string, subs []Sub) (*session, error) {
	if pu, err := url.Parse(u); err != nil || pu.Scheme != "wss" || pu.Host != "eventsub.wss.twitch.tv" {
		return nil, fmt.Errorf("refusing eventsub url %q", u)
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, u, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(512 << 10)
	_, data, err := conn.Read(dctx)
	var m wsMsg
	if err == nil {
		err = json.Unmarshal(data, &m)
	}
	if err == nil && m.Metadata.MessageType != "session_welcome" {
		err = fmt.Errorf("expected session_welcome, got %q", m.Metadata.MessageType)
	}
	if err == nil {
		err = c.subscribe(ctx, m.Payload.Session.ID, subs)
	}
	if err != nil {
		conn.CloseNow()
		return nil, err
	}
	keepalive := time.Duration(max(m.Payload.Session.KeepaliveTimeoutSeconds, 10)) * time.Second
	return &session{conn, keepalive}, nil
}

// read handles messages until the connection fails or Twitch asks for a reconnect,
// in which case it returns the already-welcomed next session.
func (c *Client) read(ctx context.Context, s *session, seen *dedupe, publish func(event.Event)) (*session, error) {
	defer s.CloseNow()
	for {
		rctx, cancel := context.WithTimeout(ctx, s.keepalive+5*time.Second)
		_, data, err := s.Read(rctx)
		cancel()
		if err != nil {
			return nil, err
		}
		var m wsMsg
		if err := json.Unmarshal(data, &m); err != nil {
			slog.Warn("bad eventsub message", "err", err)
			continue
		}
		switch m.Metadata.MessageType {
		case "notification":
			if !seen.add(m.Metadata.MessageID) || time.Since(m.Metadata.MessageTimestamp) > 10*time.Minute {
				continue // duplicate or replayed
			}
			if ev, err := toEvent(m.Metadata.SubscriptionType, m.Payload.Event); err != nil {
				slog.Warn("bad eventsub event", "type", m.Metadata.SubscriptionType, "err", err)
			} else {
				publish(ev)
			}
		case "session_reconnect":
			// ponytail: events landing on the old socket while the new one says welcome are dropped; drain it if that shows up.
			next, err := c.connect(ctx, m.Payload.Session.ReconnectURL, nil)
			s.Close(websocket.StatusNormalClosure, "")
			return next, err
		case "revocation":
			slog.Warn("twitch revoked a subscription", "type", m.Payload.Subscription.Type, "status", m.Payload.Subscription.Status)
		}
	}
}

func (c *Client) subscribe(ctx context.Context, sessionID string, subs []Sub) error {
	for _, s := range subs {
		err := c.helix(ctx, "POST", "/eventsub/subscriptions", map[string]any{
			"type": s.Type, "version": s.Version, "condition": s.Condition,
			"transport": map[string]string{"method": "websocket", "session_id": sessionID},
		}, nil)
		if errors.Is(err, ErrAuth) {
			return err
		}
		if err != nil { // e.g. points/subs on a channel that isn't affiliate yet
			slog.Warn("twitch subscription failed", "type", s.Type, "err", err)
		}
	}
	return nil
}

// toEvent maps an EventSub event to Luna's envelope, in the same shapes `luna sim` produces.
func toEvent(subType string, raw json.RawMessage) (event.Event, error) {
	var e struct {
		UserID       string          `json:"user_id"`
		UserLogin    string          `json:"user_login"`
		UserName     string          `json:"user_name"`
		ChatterID    string          `json:"chatter_user_id"`
		ChatterLogin string          `json:"chatter_user_login"`
		ChatterName  string          `json:"chatter_user_name"`
		FromID       string          `json:"from_broadcaster_user_id"`
		FromLogin    string          `json:"from_broadcaster_user_login"`
		FromName     string          `json:"from_broadcaster_user_name"`
		MessageID    string          `json:"message_id"`
		Message      json.RawMessage `json:"message"` // object in chat, string in cheer
		Badges       []struct {
			SetID string `json:"set_id"`
		} `json:"badges"`
		Tier        string `json:"tier"`
		IsGift      bool   `json:"is_gift"`
		Bits        int    `json:"bits"`
		IsAnonymous bool   `json:"is_anonymous"`
		Viewers     int    `json:"viewers"`
		UserInput   string `json:"user_input"`
		Reward      struct {
			Title string `json:"title"`
		} `json:"reward"`
		Total        int    `json:"total"`
		Cumulative   int    `json:"cumulative_months"`
		Streak       int    `json:"streak_months"`
		Title        string `json:"title"`
		CategoryName string `json:"category_name"`
		StartedAt    string `json:"started_at"`
		DurationS    int    `json:"duration_seconds"`
		IsAutomatic  bool   `json:"is_automatic"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return event.Event{}, err
	}
	user := func(id, login, name string) map[string]any {
		return map[string]any{"id": id, "login": login, "name": name, "roles": []string{}}
	}
	tier, _ := strconv.Atoi(e.Tier)
	tier /= 1000
	var typ string
	var data map[string]any
	switch subType {
	case "channel.chat.message":
		var msg struct {
			Text      string `json:"text"`
			Fragments []struct {
				Type string `json:"type"`
			} `json:"fragments"`
		}
		json.Unmarshal(e.Message, &msg)
		emotes := 0
		for _, f := range msg.Fragments {
			if f.Type == "emote" {
				emotes++
			}
		}
		roles := []string{}
		for _, b := range e.Badges {
			switch b.SetID {
			case "broadcaster", "lead_moderator", "moderator", "vip":
				roles = append(roles, b.SetID)
			case "subscriber":
				roles = append(roles, "sub")
			}
		}
		typ = "twitch.chat.message"
		data = map[string]any{"user": map[string]any{"id": e.ChatterID, "login": e.ChatterLogin, "name": e.ChatterName, "roles": roles},
			"text": msg.Text, "message_id": e.MessageID, "emotes": emotes}
	case "channel.follow":
		typ, data = "twitch.channel.follow", map[string]any{"user": user(e.UserID, e.UserLogin, e.UserName)}
	case "channel.subscribe":
		typ, data = "twitch.channel.subscribe", map[string]any{"user": user(e.UserID, e.UserLogin, e.UserName), "tier": tier, "gift": e.IsGift}
	case "channel.subscription.message":
		var msg struct {
			Text string `json:"text"`
		}
		json.Unmarshal(e.Message, &msg)
		typ, data = "twitch.channel.resub", map[string]any{"user": user(e.UserID, e.UserLogin, e.UserName), "tier": tier,
			"months": e.Cumulative, "streak": e.Streak, "message": msg.Text}
	case "channel.subscription.gift":
		typ, data = "twitch.channel.gift", map[string]any{"user": user(e.UserID, e.UserLogin, e.UserName), "tier": tier,
			"count": e.Total, "anonymous": e.IsAnonymous}
	case "stream.online":
		typ, data = "twitch.stream.online", map[string]any{"started_at": e.StartedAt}
	case "stream.offline":
		typ, data = "twitch.stream.offline", map[string]any{}
	case "channel.update":
		typ, data = "twitch.channel.update", map[string]any{"title": e.Title, "game": e.CategoryName}
	case "channel.cheer":
		var msg string
		json.Unmarshal(e.Message, &msg)
		typ, data = "twitch.channel.cheer", map[string]any{"user": user(e.UserID, e.UserLogin, e.UserName), "bits": e.Bits, "message": msg, "anonymous": e.IsAnonymous}
	case "channel.raid":
		typ, data = "twitch.channel.raid", map[string]any{"user": user(e.FromID, e.FromLogin, e.FromName), "viewers": e.Viewers}
	case "channel.channel_points_custom_reward_redemption.add":
		typ, data = "twitch.channel.redeem", map[string]any{"user": user(e.UserID, e.UserLogin, e.UserName), "reward": e.Reward.Title, "input": e.UserInput}
	case "channel.moderator.add":
		typ, data = "twitch.channel.moderator.add", map[string]any{"user": user(e.UserID, e.UserLogin, e.UserName)}
	case "channel.ad_break.begin":
		typ, data = "twitch.channel.ad_break", map[string]any{"duration_s": e.DurationS, "automatic": e.IsAutomatic}
	case "channel.moderator.remove":
		typ, data = "twitch.channel.moderator.remove", map[string]any{"user": user(e.UserID, e.UserLogin, e.UserName)}
	default:
		return event.Event{}, fmt.Errorf("unhandled subscription type %q", subType)
	}
	return event.New(typ, "twitch", false, data)
}

// dedupe remembers the last n message IDs.
type dedupe struct {
	seen  map[string]struct{}
	order []string
	next  int
}

func newDedupe(n int) *dedupe {
	return &dedupe{seen: make(map[string]struct{}, n), order: make([]string, 0, n)}
}

// add reports whether id is new.
func (d *dedupe) add(id string) bool {
	if _, ok := d.seen[id]; ok {
		return false
	}
	if len(d.order) < cap(d.order) {
		d.order = append(d.order, id)
	} else {
		delete(d.seen, d.order[d.next])
		d.order[d.next] = id
		d.next = (d.next + 1) % len(d.order)
	}
	d.seen[id] = struct{}{}
	return true
}
