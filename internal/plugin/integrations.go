package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"luna/internal/guard"
	"luna/internal/osc"
	"luna/internal/plugin/pb"
)

// OSC is the VRChat integration as plugins reach it.
type OSC interface {
	Send(address string, value any) error
	Chatbox(text string)
	Param(name string) (any, bool)
	Avatar() osc.Avatar
}

func (s *server) Osc(ctx context.Context, r *pb.OscRequest) (*pb.OscReply, error) {
	m := s.p.Manifest
	need := map[string]string{"chatbox": "chatbox", "param": "read", "avatar": "read"}[r.Action]
	switch {
	case r.Action == "send" && !m.AllowsOSCSend(r.Address):
		return nil, denied(fmt.Sprintf("%q to [permissions].osc", "send:"+r.Address))
	case r.Action != "send" && need == "":
		return nil, status.Errorf(codes.InvalidArgument, "unknown osc action %q", r.Action)
	case need != "" && !slices.Contains(m.Permissions.OSC, need):
		return nil, denied(fmt.Sprintf("%q to [permissions].osc", need))
	case s.h.OSC == nil:
		return nil, status.Error(codes.Unavailable, "OSC is off; turn it on with [osc] enabled = true in luna.toml")
	}
	sim := r.Simulated || s.p.Dev
	switch r.Action {
	case "send":
		v, err := oscValue(r, s.h.OSC.Avatar())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if sim {
			fmt.Fprintf(s.p.Log, "[sim osc] %s %v\n", r.Address, v)
			return &pb.OscReply{}, nil
		}
		if err := s.h.OSC.Send(r.Address, v); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	case "chatbox":
		text := guard.Clean(r.Text)
		if text == "" {
			return nil, status.Error(codes.InvalidArgument, "empty chatbox text")
		}
		if s.h.Guard.Blocked(text) {
			return nil, status.Error(codes.FailedPrecondition, guard.ErrBlocked.Error())
		}
		if sim {
			fmt.Fprintf(s.p.Log, "[sim chatbox] %s\n", text)
			return &pb.OscReply{}, nil
		}
		s.h.OSC.Chatbox(text)
	case "param":
		v, ok := s.h.OSC.Param(r.Name)
		if !ok {
			return &pb.OscReply{DataJson: []byte("null")}, nil
		}
		b, _ := json.Marshal(v)
		return &pb.OscReply{DataJson: b}, nil
	case "avatar":
		b, _ := json.Marshal(s.h.OSC.Avatar())
		return &pb.OscReply{DataJson: b}, nil
	}
	return &pb.OscReply{}, nil
}

// oscValue converts the request's JSON value to the OSC type VRChat expects: the type the
// plugin named, else the avatar parameter's type, else whatever the JSON looks like.
func oscValue(r *pb.OscRequest, a osc.Avatar) (any, error) {
	var v any
	if err := json.Unmarshal(r.ValueJson, &v); err != nil {
		return nil, fmt.Errorf("osc value: %w", err)
	}
	typ := r.Type
	if name, ok := strings.CutPrefix(r.Address, osc.ParamPrefix); ok && typ == "" {
		if i := slices.IndexFunc(a.Params, func(p osc.Param) bool { return p.Name == name }); i >= 0 {
			typ = strings.ToLower(a.Params[i].Type)
		}
	}
	switch x := v.(type) {
	case bool:
		if typ == "" || typ == "bool" {
			return x, nil
		}
		if typ == "int" || typ == "float" { // VRChat accepts 0/1 for a bool set as a number, and vice versa
			return map[bool]int{true: 1, false: 0}[x], nil
		}
	case string:
		if typ == "" || typ == "string" {
			return x, nil
		}
	case float64:
		switch {
		case typ == "float" || typ == "" && x != math.Trunc(x):
			return float32(x), nil
		case typ == "bool":
			return x != 0, nil
		case typ == "int" || typ == "":
			if x != math.Trunc(x) || x < math.MinInt32 || x > math.MaxInt32 {
				return nil, fmt.Errorf("osc value %v isn't an int32", x)
			}
			return int(x), nil
		}
	}
	return nil, fmt.Errorf("osc value %v can't be a %s", v, typ)
}

// HR keeps the last hour of heart rate readings, from hr.update events.
type HR struct {
	mu      sync.Mutex
	samples []*pb.HrSample
}

func (h *HR) Add(at time.Time, bpm int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.samples = append(h.samples, &pb.HrSample{AtMs: at.UnixMilli(), Bpm: int32(bpm)})
	cut := at.Add(-time.Hour).UnixMilli()
	i := 0
	for i < len(h.samples) && h.samples[i].AtMs < cut {
		i++
	}
	h.samples = h.samples[i:]
}

func (s *server) Hr(_ context.Context, r *pb.HrRequest) (*pb.HrReply, error) {
	if !s.p.Manifest.AllowsEvents("hr.update") {
		return nil, denied(`"hr.update" to [permissions].events`)
	}
	h := s.h.HR
	if h == nil {
		return &pb.HrReply{}, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := &pb.HrReply{}
	if n := len(h.samples); n > 0 {
		out.Bpm, out.AtMs = h.samples[n-1].Bpm, h.samples[n-1].AtMs
		cut := out.AtMs - int64(min(max(r.Minutes, 0), 60))*60000
		for _, smp := range h.samples {
			if r.Minutes > 0 && smp.AtMs >= cut {
				out.History = append(out.History, smp)
			}
		}
	}
	return out, nil
}

// Spotify is the streamer's Spotify, as plugins reach it.
type Spotify interface {
	Call(ctx context.Context, call string, args []byte) (any, error)
}

var spotifyCalls = map[string]string{"find": "read", "playing": "read", "in_playlist": "read", "queue": "control", "skip": "control"}

func (s *server) Spotify(ctx context.Context, r *pb.SpotifyRequest) (*pb.SpotifyReply, error) {
	need, ok := spotifyCalls[r.Call]
	switch {
	case !ok:
		return nil, status.Errorf(codes.InvalidArgument, "unknown spotify call %q", r.Call)
	case !slices.Contains(s.p.Manifest.Permissions.Spotify, need):
		return nil, denied(fmt.Sprintf("%q to [permissions].spotify", need))
	case len(r.ArgsJson) > 4<<10 || len(r.ArgsJson) > 0 && !json.Valid(r.ArgsJson):
		return nil, status.Error(codes.InvalidArgument, "spotify call arguments must be a JSON object under 4 KiB")
	case s.h.Spotify == nil:
		return nil, status.Error(codes.Unavailable, "Spotify isn't connected: set [spotify] client_id, run luna auth login spotify, and restart luna")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if need == "control" && (r.Simulated || s.p.Dev) {
		fmt.Fprintf(s.p.Log, "[sim spotify] %s %s\n", r.Call, r.ArgsJson)
		return &pb.SpotifyReply{DataJson: []byte("null")}, nil
	}
	v, err := s.h.Spotify.Call(ctx, r.Call, r.ArgsJson)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	b, _ := json.Marshal(v)
	return &pb.SpotifyReply{DataJson: b}, nil
}

// Translator translates with the streamer's DeepL key, which stays in the daemon.
type Translator interface {
	Translate(ctx context.Context, text, target string) (out, source string, err error)
}

var langRe = regexp.MustCompile(`^[A-Za-z]{2}(-[A-Za-z]{2,4})?$`)

func (s *server) Translate(ctx context.Context, r *pb.TranslateRequest) (*pb.TranslateReply, error) {
	switch {
	case !s.p.Manifest.Permissions.Translate:
		return nil, denied("translate = true to [permissions]")
	case r.Text == "" || utf8.RuneCountInString(r.Text) > 1000:
		return nil, status.Error(codes.InvalidArgument, "translate 1 to 1000 characters at a time")
	case !langRe.MatchString(r.Target):
		return nil, status.Errorf(codes.InvalidArgument, "%q isn't a language code like EN or PT-BR", r.Target)
	case s.h.Translator == nil:
		return nil, status.Error(codes.Unavailable, "translation isn't set up: run luna auth login deepl, then restart luna")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, src, err := s.h.Translator.Translate(ctx, r.Text, r.Target)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &pb.TranslateReply{Text: out, Source: src}, nil
}
