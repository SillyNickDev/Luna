package twitch

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// The calls below act as this account in its own channel, so the broadcaster's
// client is the moderator for every one of them.

func (c *Client) q(kv ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v.Encode()
}

// Ban bans userID, or times them out for seconds > 0.
func (c *Client) Ban(ctx context.Context, userID string, seconds int64, reason string) error {
	d := map[string]any{"user_id": userID, "reason": reason}
	if seconds > 0 {
		d["duration"] = min(seconds, 1209600) // Twitch's two-week maximum
	}
	return c.helix(ctx, "POST", "/moderation/bans?"+c.q("broadcaster_id", c.userID, "moderator_id", c.userID),
		map[string]any{"data": d}, nil)
}

// Unban lifts a ban or timeout.
func (c *Client) Unban(ctx context.Context, userID string) error {
	return c.helix(ctx, "DELETE", "/moderation/bans?"+c.q("broadcaster_id", c.userID, "moderator_id", c.userID, "user_id", userID), nil, nil)
}

func (c *Client) DeleteMessage(ctx context.Context, messageID string) error {
	return c.helix(ctx, "DELETE", "/moderation/chat?"+c.q("broadcaster_id", c.userID, "moderator_id", c.userID, "message_id", messageID), nil, nil)
}

// Warn sends Twitch's native warning, which the viewer must acknowledge before chatting again.
func (c *Client) Warn(ctx context.Context, userID, reason string) error {
	return c.helix(ctx, "POST", "/moderation/warnings?"+c.q("broadcaster_id", c.userID, "moderator_id", c.userID),
		map[string]any{"data": map[string]string{"user_id": userID, "reason": reason}}, nil)
}

func (c *Client) Shield(ctx context.Context, on bool) error {
	return c.helix(ctx, "PUT", "/moderation/shield_mode?"+c.q("broadcaster_id", c.userID, "moderator_id", c.userID),
		map[string]bool{"is_active": on}, nil)
}

// Shoutout is Twitch's native shoutout (2 min global and 60 min per-channel cooldowns).
func (c *Client) Shoutout(ctx context.Context, toID string) error {
	return c.helix(ctx, "POST", "/chat/shoutouts?"+c.q("from_broadcaster_id", c.userID, "to_broadcaster_id", toID, "moderator_id", c.userID), nil, nil)
}

func (c *Client) Announce(ctx context.Context, text string) error {
	return c.helix(ctx, "POST", "/chat/announcements?"+c.q("broadcaster_id", c.userID, "moderator_id", c.userID),
		map[string]string{"message": text}, nil)
}

// Whisper needs a verified phone number on this account; Twitch says so in the error if not.
func (c *Client) Whisper(ctx context.Context, toID, text string) error {
	return c.helix(ctx, "POST", "/whispers?"+c.q("from_user_id", c.userID, "to_user_id", toID), map[string]string{"message": text}, nil)
}

type User struct {
	ID        string    `json:"id"`
	Login     string    `json:"login"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// LookupUser finds a user by login, or by ID when byID is set. Not found is nil, nil.
func (c *Client) LookupUser(ctx context.Context, user string, byID bool) (*User, error) {
	var out struct {
		Data []struct {
			ID          string    `json:"id"`
			Login       string    `json:"login"`
			DisplayName string    `json:"display_name"`
			CreatedAt   time.Time `json:"created_at"`
		} `json:"data"`
	}
	key := "login"
	if byID {
		key = "id"
	}
	if err := c.helix(ctx, "GET", "/users?"+c.q(key, user), nil, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, nil
	}
	d := out.Data[0]
	return &User{ID: d.ID, Login: d.Login, Name: d.DisplayName, CreatedAt: d.CreatedAt}, nil
}

type Channel struct {
	ID    string `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
	Game  string `json:"game"`
	Title string `json:"title"`
}

func (c *Client) Channel(ctx context.Context, id string) (*Channel, error) {
	var out struct {
		Data []struct {
			ID    string `json:"broadcaster_id"`
			Login string `json:"broadcaster_login"`
			Name  string `json:"broadcaster_name"`
			Game  string `json:"game_name"`
			Title string `json:"title"`
		} `json:"data"`
	}
	if err := c.helix(ctx, "GET", "/channels?"+c.q("broadcaster_id", id), nil, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, nil
	}
	d := out.Data[0]
	return &Channel{ID: d.ID, Login: d.Login, Name: d.Name, Game: d.Game, Title: d.Title}, nil
}

type Stream struct {
	Live      bool      `json:"live"`
	StartedAt time.Time `json:"started_at"`
	Game      string    `json:"game"`
	Title     string    `json:"title"`
	Viewers   int       `json:"viewers"`
}

// Stream is the channel's current stream; Live is false when offline.
func (c *Client) Stream(ctx context.Context, id string) (*Stream, error) {
	var out struct {
		Data []struct {
			StartedAt time.Time `json:"started_at"`
			Game      string    `json:"game_name"`
			Title     string    `json:"title"`
			Viewers   int       `json:"viewer_count"`
		} `json:"data"`
	}
	if err := c.helix(ctx, "GET", "/streams?"+c.q("user_id", id), nil, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return &Stream{}, nil
	}
	d := out.Data[0]
	return &Stream{Live: true, StartedAt: d.StartedAt, Game: d.Game, Title: d.Title, Viewers: d.Viewers}, nil
}

// FollowedAt is when userID followed this channel; zero if they don't.
func (c *Client) FollowedAt(ctx context.Context, userID string) (time.Time, error) {
	var out struct {
		Data []struct {
			FollowedAt time.Time `json:"followed_at"`
		} `json:"data"`
	}
	if err := c.helix(ctx, "GET", "/channels/followers?"+c.q("broadcaster_id", c.userID, "user_id", userID), nil, &out); err != nil {
		return time.Time{}, err
	}
	if len(out.Data) == 0 {
		return time.Time{}, nil
	}
	return out.Data[0].FollowedAt, nil
}

// SendReply is SendChat as a threaded reply to parentID.
func (c *Client) SendReply(ctx context.Context, broadcasterID, text, parentID string) error {
	return c.sendChat(ctx, map[string]string{"broadcaster_id": broadcasterID, "sender_id": c.userID, "message": text, "reply_parent_message_id": parentID})
}

func (c *Client) sendChat(ctx context.Context, body map[string]string) error {
	var out struct {
		Data []struct {
			IsSent     bool `json:"is_sent"`
			DropReason *struct {
				Message string `json:"message"`
			} `json:"drop_reason"`
		} `json:"data"`
	}
	if err := c.helix(ctx, "POST", "/chat/messages", body, &out); err != nil {
		return err
	}
	if len(out.Data) == 0 || !out.Data[0].IsSent {
		reason := "unknown reason"
		if len(out.Data) > 0 && out.Data[0].DropReason != nil {
			reason = out.Data[0].DropReason.Message
		}
		return fmt.Errorf("twitch dropped the message: %s", reason)
	}
	return nil
}
