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
