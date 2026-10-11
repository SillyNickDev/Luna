package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"luna/internal/bus"
	"luna/internal/event"
	"luna/internal/guard"
	"luna/internal/plugin/pb"
	"luna/internal/store"
)

// Host is what plugins can reach, shared by every capsule's server.
type Host struct {
	Bus     *bus.Bus
	Publish func(event.Event)
	Store   *store.Store
	Guard   *guard.Guard
	Twitch  Twitch // nil while Luna runs without Twitch
	OSC     OSC    // nil while the OSC integration is off
	HR      *HR

	mu      sync.Mutex
	plugins map[string]*server
}

// Twitch is what plugins may ask of Twitch, always as the channel and through the core.
type Twitch interface {
	Mod(ctx context.Context, r *pb.ModRequest) error
	// Lookup answers a TwitchRequest call; nil means not found. args is the call's JSON object, if any.
	Lookup(ctx context.Context, call, user string, args []byte) (any, error)
}

// Plugin is one capsule as the host sees it.
type Plugin struct {
	ID       string
	Manifest *Manifest
	Config   []byte    // effective config JSON
	Dev      bool      // luna capsule dev: chat never reaches Twitch
	Log      io.Writer // the capsule's log ring
}

// Serve runs the plugin API for p on ln (the capsule's host socket) until ln closes.
func (h *Host) Serve(p Plugin, ln net.Listener) {
	s := &server{h: h, p: p, pending: map[uint64]chan *pb.CommandResult{}}
	h.mu.Lock()
	if h.plugins == nil {
		h.plugins = map[string]*server{}
	}
	h.plugins[p.ID] = s
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.plugins[p.ID] == s {
			delete(h.plugins, p.ID)
		}
		h.mu.Unlock()
	}()
	g := grpc.NewServer(grpc.MaxRecvMsgSize(1<<20), grpc.MaxConcurrentStreams(16))
	pb.RegisterHostServer(g, s)
	g.Serve(ln)
	g.Stop()
}

var ErrNotRunning = errors.New("the plugin isn't running or isn't listening for commands")

// Invoke sends a CLI or chat command to a running plugin and waits for its result.
func (h *Host) Invoke(ctx context.Context, id string, c *pb.Command) (*pb.CommandResult, error) {
	h.mu.Lock()
	s := h.plugins[id]
	h.mu.Unlock()
	if s == nil {
		return nil, ErrNotRunning
	}
	return s.invoke(ctx, c)
}

type server struct {
	pb.UnimplementedHostServer
	h *Host
	p Plugin

	mu      sync.Mutex
	cmds    chan *pb.Command // nil until the plugin opens its Commands stream
	pending map[uint64]chan *pb.CommandResult
	nextID  uint64
}

// denied names the exact manifest line to add, so the SDK can show the fix.
func denied(line string) error {
	return status.Errorf(codes.PermissionDenied, "add %s to plugin.toml", line)
}

func (s *server) Hello(_ context.Context, r *pb.HelloRequest) (*pb.HelloReply, error) {
	if r.Api != API {
		return nil, status.Errorf(codes.Unimplemented, "this Luna speaks plugin API %q, the plugin asked for %q", API, r.Api)
	}
	fmt.Fprintf(s.p.Log, "luna: plugin API connected (%s)\n", r.Sdk)
	return &pb.HelloReply{Id: s.p.ID, ConfigJson: s.p.Config, Dev: s.p.Dev}, nil
}

func match(pattern, typ string) bool {
	if p, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(typ, p)
	}
	return pattern == typ
}

func (s *server) Subscribe(r *pb.SubscribeRequest, stream grpc.ServerStreamingServer[pb.Event]) error {
	if len(r.Types) == 0 || len(r.Types) > 64 {
		return status.Error(codes.InvalidArgument, "subscribe to 1-64 event types")
	}
	exact := true
	for _, t := range r.Types {
		if !patternRe.MatchString(t) {
			return status.Errorf(codes.InvalidArgument, "bad event type %q", t)
		}
		if !s.p.Manifest.AllowsEvents(t) {
			return denied(fmt.Sprintf("%q to [permissions].events", t))
		}
		exact = exact && !strings.HasSuffix(t, "*")
	}
	var sub *bus.Sub
	if exact {
		sub = s.h.Bus.Subscribe(256, r.Types...)
	} else {
		sub = s.h.Bus.Subscribe(256) // filtered below
	}
	defer sub.Close()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case ev := <-sub.C:
			if !slices.ContainsFunc(r.Types, func(p string) bool { return match(p, ev.Type) }) {
				continue
			}
			if err := stream.Send(toPB(ev)); err != nil {
				return err
			}
		}
	}
}

func toPB(ev event.Event) *pb.Event {
	return &pb.Event{Id: ev.ID, Type: ev.Type, Source: ev.Source, TsMs: ev.TS.UnixMilli(), Simulated: ev.Simulated, DataJson: ev.Data}
}

func (s *server) Commands(stream grpc.BidiStreamingServer[pb.CommandResult, pb.Command]) error {
	cmds := make(chan *pb.Command, 16)
	s.mu.Lock()
	s.cmds = cmds
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.cmds == cmds {
			s.cmds = nil
		}
		s.mu.Unlock()
	}()
	errc := make(chan error, 1)
	go func() {
		for {
			r, err := stream.Recv()
			if err != nil {
				errc <- err
				return
			}
			s.mu.Lock()
			ch := s.pending[r.Id]
			delete(s.pending, r.Id)
			s.mu.Unlock()
			if ch != nil {
				ch <- r
			}
		}
	}()
	for {
		select {
		case err := <-errc:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case c := <-cmds:
			if err := stream.Send(c); err != nil {
				return err
			}
		}
	}
}

func (s *server) invoke(ctx context.Context, c *pb.Command) (*pb.CommandResult, error) {
	s.mu.Lock()
	if s.cmds == nil {
		s.mu.Unlock()
		return nil, ErrNotRunning
	}
	s.nextID++
	c.Id = s.nextID
	ch := make(chan *pb.CommandResult, 1)
	s.pending[c.Id] = ch
	cmds := s.cmds
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, c.Id)
		s.mu.Unlock()
	}()
	select {
	case cmds <- c:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *server) ChatSend(ctx context.Context, r *pb.ChatSendRequest) (*pb.Empty, error) {
	need := r.Mode
	switch r.Mode {
	case "":
		need = "send"
	case "announce":
	case "whisper":
		if r.ToUserId == "" {
			return nil, status.Error(codes.InvalidArgument, "a whisper needs to_user_id")
		}
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown chat mode %q", r.Mode)
	}
	if !slices.Contains(s.p.Manifest.Permissions.Chat, need) {
		return nil, denied(fmt.Sprintf("%q to [permissions].chat", need))
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := s.h.Guard.Send(ctx, guard.Msg{Text: r.Text, ReplyTo: r.ReplyTo, Mode: r.Mode, To: r.ToUserId, Simulated: r.Simulated || s.p.Dev})
	return &pb.Empty{}, twitchErr(err)
}

// twitchErr maps an outbound failure to the status the SDKs turn into their errors.
func twitchErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.ResourceExhausted, "rate limit: still queued after 30 s")
	case errors.Is(err, guard.ErrOffline):
		return status.Error(codes.Unavailable, err.Error())
	}
	return status.Error(codes.FailedPrecondition, err.Error())
}

var errPermanentBan = errors.New("permanent ban")

// modPerm is the [permissions].mod entry each action needs; lifting a timeout is part of timing out.
var modPerm = map[string]string{"timeout": "timeout", "ban": "ban", "delete": "delete", "warn": "warn",
	"shield": "shield", "shoutout": "shoutout"}

func (s *server) Mod(ctx context.Context, r *pb.ModRequest) (*pb.Empty, error) {
	need, ok := modPerm[r.Action]
	if r.Action == "unban" {
		need, ok = "ban", true
		if slices.Contains(s.p.Manifest.Permissions.Mod, "timeout") {
			need = "timeout"
		}
	}
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unknown mod action %q", r.Action)
	}
	if !slices.Contains(s.p.Manifest.Permissions.Mod, need) {
		return nil, denied(fmt.Sprintf("%q to [permissions].mod", need))
	}
	desc := r.Action + " " + r.UserId
	switch {
	case r.Action == "delete" && r.MessageId == "":
		return nil, status.Error(codes.InvalidArgument, "delete needs message_id")
	case r.Action == "delete":
		desc = "delete message " + r.MessageId
	case r.Action == "shield":
		desc = fmt.Sprintf("shield mode on=%v", r.On)
	case r.UserId == "":
		return nil, status.Errorf(codes.InvalidArgument, "%s needs user_id", r.Action)
	case r.Action == "timeout" && (r.Seconds < 1 || r.Seconds > 1209600):
		return nil, status.Error(codes.InvalidArgument, "a timeout is 1 s to 14 days")
	case r.Action == "timeout":
		desc += fmt.Sprintf(" for %ds", r.Seconds)
	}
	if r.Reason != "" {
		desc += " (" + r.Reason + ")"
	}
	fmt.Fprintf(s.p.Log, "luna: mod %s\n", desc)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := s.h.Guard.Mod(ctx, desc, r.Simulated || s.p.Dev, func(ctx context.Context) error {
		if s.h.Twitch == nil {
			return guard.ErrOffline
		}
		if r.Action == "unban" && need == "timeout" {
			// Twitch lifts bans and timeouts with one call; with only "timeout", a permanent ban stays.
			v, err := s.h.Twitch.Lookup(ctx, "ban", r.UserId, nil)
			if err != nil {
				return err
			}
			if b, _ := v.(map[string]any); b != nil && b["permanent"] == true {
				return errPermanentBan
			}
		}
		return s.h.Twitch.Mod(ctx, r)
	})
	if errors.Is(err, errPermanentBan) {
		return nil, denied(`"ban" to [permissions].mod (lifting a permanent ban)`)
	}
	return &pb.Empty{}, twitchErr(err)
}

// twitchCalls maps each Twitch call to the [permissions].twitch value it needs.
var twitchCalls = map[string]string{
	"user": "read", "follow": "read", "channel": "read", "stream": "read", "categories": "read", "schedule": "read",
	"streams": "read", "set_channel": "edit", "clip": "clips", "marker": "markers", "ads": "ads", "snooze": "ads",
	"raid": "raid", "unraid": "raid", "poll": "polls", "end_poll": "polls", "prediction": "predictions",
	"end_prediction": "predictions",
}

func (s *server) Twitch(ctx context.Context, r *pb.TwitchRequest) (*pb.TwitchReply, error) {
	need, ok := twitchCalls[r.Call]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unknown twitch call %q", r.Call)
	}
	if !slices.Contains(s.p.Manifest.Permissions.Twitch, need) {
		return nil, denied(fmt.Sprintf("%q to [permissions].twitch", need))
	}
	if (r.Call == "user" || r.Call == "follow") && r.User == "" {
		return nil, status.Errorf(codes.InvalidArgument, "%s needs user", r.Call)
	}
	if len(r.ArgsJson) > 4<<10 {
		return nil, status.Error(codes.InvalidArgument, "twitch call arguments over 4 KiB")
	}
	if len(r.ArgsJson) > 0 && !json.Valid(r.ArgsJson) {
		return nil, status.Error(codes.InvalidArgument, "twitch call arguments must be a JSON object")
	}
	if s.h.Twitch == nil {
		return nil, status.Error(codes.Unavailable, guard.ErrOffline.Error())
	}
	if strings.HasPrefix(r.User, "sim-") && need == "read" { // simulated viewers don't exist on Twitch
		return &pb.TwitchReply{DataJson: []byte("null")}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var v any
	var err error
	if need == "read" || r.Call == "ads" {
		v, err = s.h.Twitch.Lookup(ctx, r.Call, r.User, r.ArgsJson)
	} else {
		desc := strings.TrimSpace("twitch " + r.Call + " " + string(r.ArgsJson))
		sim := r.Simulated || s.p.Dev
		if sim {
			desc += " (simulated: not sent)"
		}
		fmt.Fprintf(s.p.Log, "luna: %s\n", desc)
		err = s.h.Guard.Mod(ctx, desc, sim, func(ctx context.Context) error {
			v, err = s.h.Twitch.Lookup(ctx, r.Call, r.User, r.ArgsJson)
			return err
		})
		if sim {
			v = simulatedWrites[r.Call]
		}
	}
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	b, _ := json.Marshal(v)
	return &pb.TwitchReply{DataJson: b}, nil
}

// simulatedWrites stand in for Twitch's answers, so a plugin's flow runs to the end in the simulator.
var simulatedWrites = map[string]any{
	"clip":       map[string]string{"id": "simulated", "url": "https://clips.twitch.tv/simulated", "edit_url": ""},
	"marker":     map[string]any{"id": "simulated", "position_s": 0},
	"snooze":     map[string]any{"duration_s": 0, "snooze_count": 0, "preroll_free_s": 0},
	"poll":       map[string]any{"id": "simulated", "status": "ACTIVE", "choices": []any{}},
	"prediction": map[string]any{"id": "simulated", "status": "ACTIVE", "outcomes": []any{}},
}

func (s *server) Viewer(_ context.Context, r *pb.ViewerRequest) (*pb.ViewerReply, error) {
	if !s.p.Manifest.Permissions.Viewers {
		return nil, denied("viewers = true to [permissions]")
	}
	v, err := s.h.Store.GetViewer(r.User)
	if err != nil {
		return nil, status.Error(codes.Internal, "viewer lookup failed")
	}
	b, _ := json.Marshal(v)
	return &pb.ViewerReply{DataJson: b}, nil
}

func (s *server) storeOK() error {
	if !s.p.Manifest.Permissions.Store {
		return denied("store = true to [permissions]")
	}
	return nil
}

func storeErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrStoreFull) || strings.HasPrefix(err.Error(), "store: ") {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return status.Error(codes.Internal, "store failed")
}

func (s *server) StoreGet(_ context.Context, r *pb.StoreKey) (*pb.StoreValue, error) {
	if err := s.storeOK(); err != nil {
		return nil, err
	}
	v, err := s.h.Store.PluginGet(s.p.ID, r.Key)
	if err != nil {
		return nil, storeErr(err)
	}
	return &pb.StoreValue{Found: v != nil, ValueJson: v}, nil
}

func (s *server) StoreSet(_ context.Context, r *pb.StoreSetRequest) (*pb.Empty, error) {
	if err := s.storeOK(); err != nil {
		return nil, err
	}
	if !json.Valid(r.ValueJson) {
		return nil, status.Error(codes.InvalidArgument, "value isn't JSON")
	}
	if r.TtlMs < 0 {
		return nil, status.Error(codes.InvalidArgument, "ttl can't be negative")
	}
	return &pb.Empty{}, storeErr(s.h.Store.PluginSet(s.p.ID, r.Key, r.ValueJson, time.Duration(r.TtlMs)*time.Millisecond))
}

func (s *server) StoreBatch(_ context.Context, r *pb.StoreBatchRequest) (*pb.Empty, error) {
	if err := s.storeOK(); err != nil {
		return nil, err
	}
	if len(r.Set)+len(r.Delete) > 500 {
		return nil, status.Error(codes.InvalidArgument, "a batch holds at most 500 writes")
	}
	sets := make([]store.KV, len(r.Set))
	for i, kv := range r.Set {
		if !json.Valid(kv.ValueJson) || kv.TtlMs < 0 {
			return nil, status.Errorf(codes.InvalidArgument, "%s: value isn't JSON or ttl is negative", kv.Key)
		}
		sets[i] = store.KV{Key: kv.Key, Value: kv.ValueJson, TTL: time.Duration(kv.TtlMs) * time.Millisecond}
	}
	return &pb.Empty{}, storeErr(s.h.Store.PluginBatch(s.p.ID, sets, r.Delete))
}

func (s *server) StoreDelete(_ context.Context, r *pb.StoreKey) (*pb.Empty, error) {
	if err := s.storeOK(); err != nil {
		return nil, err
	}
	return &pb.Empty{}, storeErr(s.h.Store.PluginDelete(s.p.ID, r.Key))
}

func (s *server) StoreIncr(_ context.Context, r *pb.StoreIncrRequest) (*pb.StoreIncrReply, error) {
	if err := s.storeOK(); err != nil {
		return nil, err
	}
	n, err := s.h.Store.PluginIncr(s.p.ID, r.Key, r.By)
	return &pb.StoreIncrReply{Value: n}, storeErr(err)
}

func (s *server) StoreKeys(_ context.Context, r *pb.StoreKeysRequest) (*pb.StoreKeysReply, error) {
	if err := s.storeOK(); err != nil {
		return nil, err
	}
	keys, err := s.h.Store.PluginKeys(s.p.ID, r.Prefix)
	return &pb.StoreKeysReply{Keys: keys}, storeErr(err)
}

func (s *server) Log(_ context.Context, r *pb.LogRequest) (*pb.Empty, error) {
	lvl := r.Level
	switch lvl {
	case "debug", "info", "warn", "error":
	default:
		lvl = "info"
	}
	fmt.Fprintf(s.p.Log, "[%s] %s\n", lvl, r.Msg)
	return &pb.Empty{}, nil
}

var emitRe = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)*$`)

func (s *server) Emit(_ context.Context, r *pb.EmitRequest) (*pb.Empty, error) {
	if !emitRe.MatchString(r.Name) || len(r.Name) > 64 {
		return nil, status.Error(codes.InvalidArgument, "event name must be lowercase dotted words, ≤64 chars")
	}
	var obj map[string]any
	if len(r.DataJson) > 64<<10 || json.Unmarshal(r.DataJson, &obj) != nil {
		return nil, status.Error(codes.InvalidArgument, "event data must be a JSON object ≤64 KiB")
	}
	e, _ := event.New("plugin."+s.p.ID+"."+r.Name, "plugin:"+s.p.ID, s.p.Dev, nil)
	e.Data = r.DataJson
	s.h.Publish(e)
	return &pb.Empty{}, nil
}

// refusal answers every call with the reason Luna won't serve this plugin (a bad
// [plugins] setting, say), so the plugin logs why instead of a missing socket.
type refusal struct {
	pb.UnimplementedHostServer
	err error
}

func (r refusal) Hello(context.Context, *pb.HelloRequest) (*pb.HelloReply, error) {
	return nil, status.Error(codes.FailedPrecondition, r.err.Error())
}

// ServeRefusal serves only that refusal on ln until it closes.
func ServeRefusal(ln net.Listener, err error) {
	g := grpc.NewServer()
	pb.RegisterHostServer(g, refusal{err: err})
	g.Serve(ln)
	g.Stop()
}
