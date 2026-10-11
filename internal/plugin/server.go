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

	mu      sync.Mutex
	plugins map[string]*server
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
	if !slices.Contains(s.p.Manifest.Permissions.Chat, "send") {
		return nil, denied(`"send" to [permissions].chat`)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.h.Guard.Chat(ctx, r.Text, r.Simulated || s.p.Dev); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, status.Error(codes.ResourceExhausted, "chat rate limit: still queued after 30 s")
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &pb.Empty{}, nil
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
