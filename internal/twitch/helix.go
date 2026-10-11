package twitch

import (
	"context"
	"errors"
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

// ---------- stream management (Stream tools) ----------

// SetChannel changes the title and/or category; empty fields stay as they are.
func (c *Client) SetChannel(ctx context.Context, title, gameID string) error {
	body := map[string]string{}
	if title != "" {
		body["title"] = title
	}
	if gameID != "" {
		body["game_id"] = gameID
	}
	return c.helix(ctx, "PATCH", "/channels?"+c.q("broadcaster_id", c.userID), body, nil)
}

type Category struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *Client) SearchCategories(ctx context.Context, query string) ([]Category, error) {
	var out struct {
		Data []Category `json:"data"`
	}
	err := c.helix(ctx, "GET", "/search/categories?"+c.q("query", query, "first", "10"), nil, &out)
	return out.Data, err
}

type Clip struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	EditURL string `json:"edit_url"`
}

// Clip captures the last ~30 s of the live stream. Twitch makes it asynchronously, so the
// link can take a few seconds to work.
func (c *Client) Clip(ctx context.Context) (*Clip, error) {
	var out struct {
		Data []Clip `json:"data"`
	}
	if err := c.helix(ctx, "POST", "/clips?"+c.q("broadcaster_id", c.userID), nil, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, fmt.Errorf("twitch made no clip")
	}
	cl := out.Data[0]
	cl.URL = "https://clips.twitch.tv/" + cl.ID
	return &cl, nil
}

type Marker struct {
	ID        string `json:"id"`
	PositionS int    `json:"position_s"`
}

func (c *Client) Marker(ctx context.Context, description string) (*Marker, error) {
	var out struct {
		Data []struct {
			ID        string `json:"id"`
			PositionS int    `json:"position_seconds"`
		} `json:"data"`
	}
	if err := c.helix(ctx, "POST", "/streams/markers", map[string]string{"user_id": c.userID, "description": description}, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, fmt.Errorf("twitch made no marker")
	}
	return &Marker{ID: out.Data[0].ID, PositionS: out.Data[0].PositionS}, nil
}

type AdSchedule struct {
	NextAdAt    time.Time `json:"next_ad_at,omitzero"`
	LastAdAt    time.Time `json:"last_ad_at,omitzero"`
	DurationS   int       `json:"duration_s"`
	SnoozeCount int       `json:"snooze_count"`
	PrerollS    int       `json:"preroll_free_s"`
}

type adReply struct {
	Data []struct {
		NextAdAt    int64 `json:"next_ad_at"` // Unix seconds; 0 when none is scheduled
		LastAdAt    int64 `json:"last_ad_at"`
		Duration    int   `json:"duration"`
		SnoozeCount int   `json:"snooze_count"`
		Preroll     int   `json:"preroll_free_time"`
	} `json:"data"`
}

func (r adReply) schedule() *AdSchedule {
	if len(r.Data) == 0 {
		return &AdSchedule{}
	}
	d := r.Data[0]
	s := &AdSchedule{DurationS: d.Duration, SnoozeCount: d.SnoozeCount, PrerollS: d.Preroll}
	if d.NextAdAt > 0 {
		s.NextAdAt = time.Unix(d.NextAdAt, 0).UTC()
	}
	if d.LastAdAt > 0 {
		s.LastAdAt = time.Unix(d.LastAdAt, 0).UTC()
	}
	return s
}

func (c *Client) Ads(ctx context.Context) (*AdSchedule, error) {
	var out adReply
	err := c.helix(ctx, "GET", "/channels/ads?"+c.q("broadcaster_id", c.userID), nil, &out)
	return out.schedule(), err
}

// SnoozeAd pushes the next scheduled ad back 5 minutes (Twitch allows a few per hour).
func (c *Client) SnoozeAd(ctx context.Context) (*AdSchedule, error) {
	var out adReply
	err := c.helix(ctx, "POST", "/channels/ads/schedule/snooze?"+c.q("broadcaster_id", c.userID), nil, &out)
	return out.schedule(), err
}

type Segment struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end,omitzero"`
	Title    string    `json:"title"`
	Category string    `json:"category,omitempty"`
}

// Schedule is the channel's upcoming schedule; empty if it has none.
func (c *Client) Schedule(ctx context.Context) ([]Segment, error) {
	var out struct {
		Data struct {
			Segments []struct {
				Start    time.Time  `json:"start_time"`
				End      time.Time  `json:"end_time"`
				Title    string     `json:"title"`
				Canceled *time.Time `json:"canceled_until"`
				Category *struct {
					Name string `json:"name"`
				} `json:"category"`
			} `json:"segments"`
		} `json:"data"`
	}
	err := c.helix(ctx, "GET", "/schedule?"+c.q("broadcaster_id", c.userID, "first", "5"), nil, &out)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == 404 { // no schedule set up
		return nil, nil
	}
	var segs []Segment
	for _, s := range out.Data.Segments {
		if s.Canceled != nil {
			continue
		}
		seg := Segment{Start: s.Start, End: s.End, Title: s.Title}
		if s.Category != nil {
			seg.Category = s.Category.Name
		}
		segs = append(segs, seg)
	}
	return segs, err
}

type LiveStream struct {
	UserID  string `json:"user_id"`
	Login   string `json:"login"`
	Name    string `json:"name"`
	Game    string `json:"game"`
	Title   string `json:"title"`
	Viewers int    `json:"viewers"`
}

// Streams lists which of logins (up to 100) are live.
func (c *Client) Streams(ctx context.Context, logins []string) ([]LiveStream, error) {
	v := url.Values{}
	for _, l := range logins[:min(len(logins), 100)] {
		v.Add("user_login", l)
	}
	var out struct {
		Data []struct {
			UserID  string `json:"user_id"`
			Login   string `json:"user_login"`
			Name    string `json:"user_name"`
			Game    string `json:"game_name"`
			Title   string `json:"title"`
			Viewers int    `json:"viewer_count"`
		} `json:"data"`
	}
	if err := c.helix(ctx, "GET", "/streams?"+v.Encode(), nil, &out); err != nil {
		return nil, err
	}
	var live []LiveStream
	for _, d := range out.Data {
		live = append(live, LiveStream(d))
	}
	return live, nil
}

// Raid starts a raid on toID; Twitch counts down 90 s before it goes.
func (c *Client) Raid(ctx context.Context, toID string) error {
	return c.helix(ctx, "POST", "/raids?"+c.q("from_broadcaster_id", c.userID, "to_broadcaster_id", toID), nil, nil)
}

func (c *Client) Unraid(ctx context.Context) error {
	return c.helix(ctx, "DELETE", "/raids?"+c.q("broadcaster_id", c.userID), nil, nil)
}
