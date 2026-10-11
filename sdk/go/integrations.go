package luna

import (
	"encoding/json"
	"fmt"
	"time"

	"luna/internal/plugin/pb"
)

// OSC talks to VRChat through Luna; needs [permissions].osc.
type OSC struct{ b *Bot }

type AvatarParam struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // Bool, Int, Float
	Writable bool   `json:"writable"`
}

type Avatar struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Params []AvatarParam `json:"params"`
}

// Has reports whether the avatar has a parameter Luna can set.
func (a *Avatar) Has(name string) bool {
	for _, p := range a.Params {
		if p.Name == name && p.Writable {
			return true
		}
	}
	return false
}

// Send sets an OSC address to value: a bool, int, float32/float64, string, or json.Number to
// let Luna pick Int or Float from the avatar. Coalesced per
// address per frame, so only the latest value in each 1/30 s goes out.
func (o OSC) Send(c *Ctx, address string, value any) error {
	typ := ""
	switch value.(type) {
	case bool:
		typ = "bool"
	case int, int32, int64:
		typ = "int"
	case float32, float64:
		typ = "float"
	case string:
		typ = "string"
	case json.Number: // a number of no fixed type: Luna picks Int or Float from the avatar's parameter
	default:
		return fmt.Errorf("luna: OSC can't send a %T", value)
	}
	raw, _ := json.Marshal(value)
	_, err := o.b.host.Osc(c, &pb.OscRequest{Action: "send", Address: address, ValueJson: raw, Type: typ, Simulated: c.simulated()})
	return wrap(err)
}

// SetParam is Send to /avatar/parameters/<name>.
func (o OSC) SetParam(c *Ctx, name string, value any) error {
	return o.Send(c, "/avatar/parameters/"+name, value)
}

// Chatbox shows text in the VRChat chatbox (144 characters; rate-limited, latest wins).
func (o OSC) Chatbox(c *Ctx, text string) error {
	_, err := o.b.host.Osc(c, &pb.OscRequest{Action: "chatbox", Text: text, Simulated: c.simulated()})
	return wrap(err)
}

// Param is VRChat's last reported value of an avatar parameter (bool, float64 or nil).
func (o OSC) Param(c *Ctx, name string) (any, error) {
	r, err := o.b.host.Osc(c, &pb.OscRequest{Action: "param", Name: name})
	if err != nil {
		return nil, wrap(err)
	}
	var v any
	return v, json.Unmarshal(r.DataJson, &v)
}

// Avatar is the avatar being worn and its parameters (empty before VRChat reports one).
func (o OSC) Avatar(c *Ctx) (*Avatar, error) {
	r, err := o.b.host.Osc(c, &pb.OscRequest{Action: "avatar"})
	if err != nil {
		return nil, wrap(err)
	}
	var a Avatar
	return &a, json.Unmarshal(r.DataJson, &a)
}

// HR is the streamer's heart rate; needs hr.update in [permissions].events.
type HR struct{ b *Bot }

type HRSample struct {
	At  time.Time
	BPM int
}

// BPM is the latest reading, 0 before the first, with when it was taken.
func (h HR) BPM(c *Ctx) (int, time.Time, error) {
	r, err := h.b.host.Hr(c, &pb.HrRequest{})
	if err != nil {
		return 0, time.Time{}, wrap(err)
	}
	if r.AtMs == 0 {
		return 0, time.Time{}, nil
	}
	return int(r.Bpm), time.UnixMilli(r.AtMs).UTC(), nil
}

// History is the readings from the last minutes (up to 60), oldest first.
func (h HR) History(c *Ctx, minutes int) ([]HRSample, error) {
	r, err := h.b.host.Hr(c, &pb.HrRequest{Minutes: int32(minutes)})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]HRSample, len(r.History))
	for i, s := range r.History {
		out[i] = HRSample{time.UnixMilli(s.AtMs).UTC(), int(s.Bpm)}
	}
	return out, nil
}

// Spotify is the streamer's Spotify, through Luna; needs [permissions].spotify.
type Spotify struct{ b *Bot }

type Track struct {
	ID         string `json:"id"`
	URI        string `json:"uri"`
	Name       string `json:"name"`
	Artists    string `json:"artists"`
	DurationMS int    `json:"duration_ms"`
	Explicit   bool   `json:"explicit"`
}

type Playing struct {
	Track     *Track `json:"track"`
	IsPlaying bool   `json:"is_playing"`
}

func (s Spotify) call(c *Ctx, call string, args, out any) error {
	req := &pb.SpotifyRequest{Call: call, Simulated: c.simulated()}
	if args != nil {
		req.ArgsJson, _ = json.Marshal(args)
	}
	r, err := s.b.host.Spotify(c, req)
	if err != nil {
		return wrap(err)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(r.DataJson, out)
}

// Find is the track a request names: a Spotify link, or the top search result (nil if none). Needs "read".
func (s Spotify) Find(c *Ctx, query string) (*Track, error) {
	var t *Track
	return t, s.call(c, "find", map[string]string{"query": query}, &t)
}

// Playing is what's playing now. Needs "read".
func (s Spotify) Playing(c *Ctx) (*Playing, error) {
	var p Playing
	return &p, s.call(c, "playing", nil, &p)
}

// InPlaylist reports whether a track is in a playlist (link, URI or ID; cached for 10 minutes). Needs "read".
func (s Spotify) InPlaylist(c *Ctx, playlist, trackID string) (bool, error) {
	var r struct {
		In bool `json:"in"`
	}
	return r.In, s.call(c, "in_playlist", map[string]string{"playlist": playlist, "track": trackID}, &r)
}

// Queue adds a track to the streamer's Spotify queue. Needs "control".
func (s Spotify) Queue(c *Ctx, uri string) error {
	return s.call(c, "queue", map[string]string{"uri": uri}, nil)
}

// Skip skips to the next song. Needs "control".
func (s Spotify) Skip(c *Ctx) error { return s.call(c, "skip", nil, nil) }

// Translate turns text into the target language ("EN", "DE", "PT-BR", ...) with the
// streamer's DeepL key, and says which language it was in. Needs translate = true.
func (b *Bot) Translate(c *Ctx, text, target string) (out, source string, err error) {
	r, err := b.host.Translate(c, &pb.TranslateRequest{Text: text, Target: target})
	if err != nil {
		return "", "", wrap(err)
	}
	return r.Text, r.Source, nil
}
