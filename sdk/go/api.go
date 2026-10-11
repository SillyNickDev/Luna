package luna

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"luna/internal/plugin/pb"
)

// wrap turns a gRPC status into the SDK's errors, keeping Luna's message (which names
// the fix, e.g. the manifest line to add).
func wrap(err error) error {
	if err == nil {
		return nil
	}
	s, _ := status.FromError(err)
	var kind error
	switch s.Code() {
	case codes.PermissionDenied:
		kind = ErrPermissionDenied
	case codes.ResourceExhausted:
		kind = ErrRateLimited
	case codes.Unavailable:
		kind = ErrNotConnected
	case codes.DeadlineExceeded:
		kind = ErrTimeout
	default:
		return fmt.Errorf("luna: %s", s.Message())
	}
	return fmt.Errorf("%w: %s", kind, s.Message())
}

// Chat sends messages through Luna's output guard. Answers to simulated events go to
// the simulator, never to Twitch.
type Chat struct{ b *Bot }

func (ch Chat) send(c *Ctx, r *pb.ChatSendRequest) error {
	r.Simulated = c.simulated()
	_, err := ch.b.host.ChatSend(c, r)
	return wrap(err)
}

func (ch Chat) Send(c *Ctx, text string) error {
	return ch.send(c, &pb.ChatSendRequest{Text: text})
}

// Announce posts a highlighted announcement; needs "announce" in [permissions].chat.
func (ch Chat) Announce(c *Ctx, text string) error {
	return ch.send(c, &pb.ChatSendRequest{Text: text, Mode: "announce"})
}

// Whisper messages one viewer privately; needs "whisper" in [permissions].chat.
func (ch Chat) Whisper(c *Ctx, userID, text string) error {
	return ch.send(c, &pb.ChatSendRequest{Text: text, Mode: "whisper", ToUserId: userID})
}

// Mod acts as the channel's moderator; each action needs its name in [permissions].mod.
type Mod struct{ b *Bot }

func (m Mod) do(c *Ctx, r *pb.ModRequest) error {
	r.Simulated = c.simulated()
	_, err := m.b.host.Mod(c, r)
	return wrap(err)
}

func (m Mod) Timeout(c *Ctx, userID string, d time.Duration, reason string) error {
	return m.do(c, &pb.ModRequest{Action: "timeout", UserId: userID, Seconds: int64(max(d, time.Second) / time.Second), Reason: reason})
}
func (m Mod) Ban(c *Ctx, userID, reason string) error {
	return m.do(c, &pb.ModRequest{Action: "ban", UserId: userID, Reason: reason})
}

// Unban lifts a ban or a timeout.
func (m Mod) Unban(c *Ctx, userID string) error {
	return m.do(c, &pb.ModRequest{Action: "unban", UserId: userID})
}
func (m Mod) Delete(c *Ctx, messageID string) error {
	return m.do(c, &pb.ModRequest{Action: "delete", MessageId: messageID})
}
func (m Mod) Warn(c *Ctx, userID, reason string) error {
	return m.do(c, &pb.ModRequest{Action: "warn", UserId: userID, Reason: reason})
}
func (m Mod) Shield(c *Ctx, on bool) error {
	return m.do(c, &pb.ModRequest{Action: "shield", On: on})
}

// Shoutout is Twitch's native shoutout of channelID.
func (m Mod) Shoutout(c *Ctx, channelID string) error {
	return m.do(c, &pb.ModRequest{Action: "shoutout", UserId: channelID})
}

// Store is the plugin's private key-value data, kept by Luna. Values are JSON.
type Store struct{ b *Bot }

// Get decodes key into v and reports whether it was there.
func (s Store) Get(c *Ctx, key string, v any) (bool, error) {
	r, err := s.b.host.StoreGet(c, &pb.StoreKey{Key: key})
	if err != nil || !r.Found {
		return false, wrap(err)
	}
	return true, json.Unmarshal(r.ValueJson, v)
}

// Set stores v; ttl 0 keeps it forever.
func (s Store) Set(c *Ctx, key string, v any, ttl time.Duration) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.b.host.StoreSet(c, &pb.StoreSetRequest{Key: key, ValueJson: raw, TtlMs: ttl.Milliseconds()})
	return wrap(err)
}

func (s Store) Delete(c *Ctx, key string) error {
	_, err := s.b.host.StoreDelete(c, &pb.StoreKey{Key: key})
	return wrap(err)
}

// Incr adds by to an integer value atomically and returns the result.
func (s Store) Incr(c *Ctx, key string, by int64) (int64, error) {
	r, err := s.b.host.StoreIncr(c, &pb.StoreIncrRequest{Key: key, By: by})
	if err != nil {
		return 0, wrap(err)
	}
	return r.Value, nil
}

func (s Store) Keys(c *Ctx, prefix string) ([]string, error) {
	r, err := s.b.host.StoreKeys(c, &pb.StoreKeysRequest{Prefix: prefix})
	if err != nil {
		return nil, wrap(err)
	}
	return r.Keys, nil
}

// Batch groups writes: all of them land, or none do.
type Batch struct {
	sets    []*pb.StoreSetRequest
	deletes []string
	err     error
}

func (bt *Batch) Set(key string, v any, ttl time.Duration) {
	raw, err := json.Marshal(v)
	if err != nil && bt.err == nil {
		bt.err = err
	}
	bt.sets = append(bt.sets, &pb.StoreSetRequest{Key: key, ValueJson: raw, TtlMs: ttl.Milliseconds()})
}

func (bt *Batch) Delete(key string) { bt.deletes = append(bt.deletes, key) }

func (s Store) Apply(c *Ctx, bt *Batch) error {
	if bt.err != nil {
		return bt.err
	}
	_, err := s.b.host.StoreBatch(c, &pb.StoreBatchRequest{Set: bt.sets, Delete: bt.deletes})
	return wrap(err)
}

// Twitch reads public facts; needs "read" in [permissions].twitch.
type Twitch struct{ b *Bot }

type TwitchUser struct {
	ID        string    `json:"id"`
	Login     string    `json:"login"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type Channel struct {
	ID    string `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
	Game  string `json:"game"`
	Title string `json:"title"`
}

type Stream struct {
	Live      bool      `json:"live"`
	StartedAt time.Time `json:"started_at"`
	Game      string    `json:"game"`
	Title     string    `json:"title"`
	Viewers   int       `json:"viewers"`
}

func (t Twitch) get(c *Ctx, call, user string, v any) (bool, error) {
	return t.do(c, call, user, nil, v)
}

// do makes a Twitch call with args; writes answering a simulated event are only printed.
func (t Twitch) do(c *Ctx, call, user string, args any, v any) (bool, error) {
	req := &pb.TwitchRequest{Call: call, User: user, Simulated: c.simulated()}
	if args != nil {
		var err error
		if req.ArgsJson, err = json.Marshal(args); err != nil {
			return false, err
		}
	}
	r, err := t.b.host.Twitch(c, req)
	if err != nil {
		return false, wrap(err)
	}
	if string(r.DataJson) == "null" {
		return false, nil
	}
	return true, json.Unmarshal(r.DataJson, v)
}

// User looks someone up by login or numeric ID; nil if there's no such user.
func (t Twitch) User(c *Ctx, user string) (*TwitchUser, error) {
	var u TwitchUser
	ok, err := t.get(c, "user", user, &u)
	if !ok {
		return nil, err
	}
	return &u, err
}

// Channel is a channel's category and title; empty id means this channel.
func (t Twitch) Channel(c *Ctx, id string) (*Channel, error) {
	var ch Channel
	ok, err := t.get(c, "channel", id, &ch)
	if !ok {
		return nil, err
	}
	return &ch, err
}

// Stream is a channel's live stream (Live false when offline); empty id means this channel.
func (t Twitch) Stream(c *Ctx, id string) (*Stream, error) {
	var s Stream
	_, err := t.get(c, "stream", id, &s)
	return &s, err
}

// FollowedAt is when userID followed this channel; zero if they don't.
func (t Twitch) FollowedAt(c *Ctx, userID string) (time.Time, error) {
	var f struct {
		At time.Time `json:"followed_at"`
	}
	_, err := t.get(c, "follow", userID, &f)
	return f.At, err
}

type Category struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// SearchCategories finds Twitch categories (games) matching query, best first.
func (t Twitch) SearchCategories(c *Ctx, query string) ([]Category, error) {
	var out []Category
	_, err := t.do(c, "categories", "", map[string]string{"query": query}, &out)
	return out, err
}

type Segment struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Title    string    `json:"title"`
	Category string    `json:"category"`
}

// Schedule is this channel's upcoming Twitch schedule, soonest first; empty if it has none.
func (t Twitch) Schedule(c *Ctx) ([]Segment, error) {
	var out []Segment
	_, err := t.do(c, "schedule", "", nil, &out)
	return out, err
}

type LiveStream struct {
	UserID  string `json:"user_id"`
	Login   string `json:"login"`
	Name    string `json:"name"`
	Game    string `json:"game"`
	Title   string `json:"title"`
	Viewers int    `json:"viewers"`
}

// Streams says which of logins (up to 100) are live right now.
func (t Twitch) Streams(c *Ctx, logins []string) ([]LiveStream, error) {
	var out []LiveStream
	_, err := t.do(c, "streams", "", map[string][]string{"logins": logins}, &out)
	return out, err
}

// SetChannel changes this channel's title and/or category (by ID, from SearchCategories);
// an empty one stays as it is. Needs "edit" in [permissions].twitch.
func (t Twitch) SetChannel(c *Ctx, title, gameID string) error {
	_, err := t.do(c, "set_channel", "", map[string]string{"title": title, "game_id": gameID}, nil)
	return err
}

type Clip struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	EditURL string `json:"edit_url"`
}

// Clip clips the last ~30 s of the live stream. Needs "clips".
func (t Twitch) Clip(c *Ctx) (*Clip, error) {
	var out Clip
	_, err := t.do(c, "clip", "", nil, &out)
	return &out, err
}

type Marker struct {
	ID        string `json:"id"`
	PositionS int    `json:"position_s"`
}

// Marker drops a VOD marker with a note. Needs "markers".
func (t Twitch) Marker(c *Ctx, description string) (*Marker, error) {
	var out Marker
	_, err := t.do(c, "marker", "", map[string]string{"description": description}, &out)
	return &out, err
}

type AdSchedule struct {
	NextAdAt    time.Time `json:"next_ad_at"` // zero: none scheduled
	LastAdAt    time.Time `json:"last_ad_at"`
	DurationS   int       `json:"duration_s"`
	SnoozeCount int       `json:"snooze_count"` // snoozes left
	PrerollS    int       `json:"preroll_free_s"`
}

// Ads is the channel's ad schedule. Needs "ads".
func (t Twitch) Ads(c *Ctx) (*AdSchedule, error) {
	var out AdSchedule
	_, err := t.do(c, "ads", "", nil, &out)
	return &out, err
}

// SnoozeAd pushes the next ad back 5 minutes, if snoozes are left. Needs "ads".
func (t Twitch) SnoozeAd(c *Ctx) (*AdSchedule, error) {
	var out AdSchedule
	_, err := t.do(c, "snooze", "", nil, &out)
	return &out, err
}

// Raid starts a raid on the channel with user ID toID (Twitch counts down 90 s). Needs "raid".
func (t Twitch) Raid(c *Ctx, toID string) error {
	_, err := t.do(c, "raid", "", map[string]string{"to_id": toID}, nil)
	return err
}

// Unraid cancels a pending raid. Needs "raid".
func (t Twitch) Unraid(c *Ctx) error {
	_, err := t.do(c, "unraid", "", nil, nil)
	return err
}

type Choice struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Votes int    `json:"votes"` // predictions: how many users picked it
}

type Poll struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Status  string   `json:"status"`
	Choices []Choice `json:"choices"`
}

type Prediction struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Status   string   `json:"status"`
	Outcomes []Choice `json:"outcomes"`
}

// StartPoll runs a native Twitch poll (affiliates and partners) for d (15 s to 30 min). Needs "polls".
func (t Twitch) StartPoll(c *Ctx, title string, choices []string, d time.Duration) (*Poll, error) {
	var out Poll
	_, err := t.do(c, "poll", "", map[string]any{"title": title, "choices": choices, "duration_s": int(d.Seconds())}, &out)
	return &out, err
}

// EndPoll ends a poll now and returns its votes. Needs "polls".
func (t Twitch) EndPoll(c *Ctx, id string) (*Poll, error) {
	var out Poll
	_, err := t.do(c, "end_poll", "", map[string]any{"id": id}, &out)
	return &out, err
}

// StartPrediction opens a native prediction for d (30 s to 30 min). Needs "predictions".
func (t Twitch) StartPrediction(c *Ctx, title string, outcomes []string, d time.Duration) (*Prediction, error) {
	var out Prediction
	_, err := t.do(c, "prediction", "", map[string]any{"title": title, "choices": outcomes, "duration_s": int(d.Seconds())}, &out)
	return &out, err
}

// EndPrediction locks ("LOCKED"), resolves ("RESOLVED", with the winning outcome's ID) or
// cancels ("CANCELED") a prediction. Needs "predictions".
func (t Twitch) EndPrediction(c *Ctx, id, status, winnerID string) (*Prediction, error) {
	var out Prediction
	_, err := t.do(c, "end_prediction", "", map[string]any{"id": id, "status": status, "winner": winnerID}, &out)
	return &out, err
}

// Viewers is Luna's shared viewer profile; needs viewers = true.
type Viewers struct{ b *Bot }

type Viewer struct {
	ID        string    `json:"id"`
	Login     string    `json:"login"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Messages  int64     `json:"messages"`
	WatchS    int64     `json:"watch_s"` // time spent chatting while live
	Streams   int64     `json:"streams"`
	Streak    int64     `json:"streak"` // consecutive streams
}

// Get finds a viewer by user ID or login; nil if Luna has never seen them chat.
func (v Viewers) Get(c *Ctx, user string) (*Viewer, error) {
	r, err := v.b.host.Viewer(c, &pb.ViewerRequest{User: user})
	if err != nil {
		return nil, wrap(err)
	}
	if string(r.DataJson) == "null" {
		return nil, nil
	}
	var out Viewer
	return &out, json.Unmarshal(r.DataJson, &out)
}

// Log writes to the plugin's log (luna capsule logs <name>).
type Log struct{ b *Bot }

func (l Log) log(c *Ctx, level, msg string) {
	if l.b.host == nil {
		log.Printf("[%s] %s", level, msg)
		return
	}
	if _, err := l.b.host.Log(c, &pb.LogRequest{Level: level, Msg: msg}); err != nil {
		log.Printf("[%s] %s", level, msg)
	}
}

func (l Log) Debug(c *Ctx, msg string) { l.log(c, "debug", msg) }
func (l Log) Info(c *Ctx, msg string)  { l.log(c, "info", msg) }
func (l Log) Warn(c *Ctx, msg string)  { l.log(c, "warn", msg) }
func (l Log) Error(c *Ctx, msg string) { l.log(c, "error", msg) }
