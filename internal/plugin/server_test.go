package plugin

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"luna/internal/bus"
	"luna/internal/config"
	"luna/internal/event"
	"luna/internal/guard"
	"luna/internal/plugin/pb"
	"luna/internal/store"
)

const testPlugin = `id = "dev.test.hrc"
version = "0.1.0"
api = "1"
[capsule]
runtime = "native"
entry = "x"
[permissions]
events = ["twitch.*"]
chat = ["send"]
store = true
[cli]
name = "hrc"
[[cli.commands]]
name = "threshold"
args = [{ name = "bpm", type = "int", optional = true }]
[[chat.commands]]
name = "hr"
cooldown = { global = "10s" }
[config]
threshold = { type = "int", default = 140 }
`

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func setup(t *testing.T, manifest string) (*Host, pb.HostClient, *syncBuf, *[]string) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "plugin.toml"), []byte(manifest), 0o644)
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.ID = "dev.test.hrc"
	st, err := store.Open(filepath.Join(t.TempDir(), "luna.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var sent []string
	var mu sync.Mutex
	g := guard.New(func() *config.Config { return config.Default() })
	g.Sim = func(_ context.Context, m guard.Msg) error {
		mu.Lock()
		sent = append(sent, m.Text)
		mu.Unlock()
		return nil
	}
	b := bus.New()
	h := &Host{Bus: b, Publish: b.Publish, Store: st, Guard: g}
	sock, _ := os.MkdirTemp("", "lp")
	t.Cleanup(func() { os.RemoveAll(sock) })
	ln, err := net.Listen("unix", filepath.Join(sock, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	cfg, _ := m.EffectiveConfig(map[string]any{"threshold": int64(150)})
	go h.Serve(Plugin{ID: m.ID, Manifest: m, Config: cfg, Dev: true, Log: logs}, ln)
	t.Cleanup(func() { ln.Close() })
	conn, err := grpc.NewClient("unix://"+filepath.Join(sock, "host.sock"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return h, pb.NewHostClient(conn), logs, &sent
}

func TestHostAPI(t *testing.T) {
	h, c, logs, sent := setup(t, testPlugin)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hello, err := c.Hello(ctx, &pb.HelloRequest{Api: "1", Sdk: "test"})
	if err != nil || hello.Id != "dev.test.hrc" || string(hello.ConfigJson) != `{"threshold":150}` || !hello.Dev {
		t.Fatalf("hello: %+v %v", hello, err)
	}
	denied, _ := c.Subscribe(ctx, &pb.SubscribeRequest{Types: []string{"hr.update"}})
	if _, err := denied.Recv(); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), `"hr.update" to [permissions].events`) {
		t.Fatalf("subscribe outside permissions: %v", err)
	}
	stream, err := c.Subscribe(ctx, &pb.SubscribeRequest{Types: []string{"twitch.follow"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the subscription register
	for _, typ := range []string{"twitch.raid", "twitch.follow"} {
		ev, _ := event.New(typ, "sim", true, map[string]any{"user": map[string]string{"name": "fox"}})
		h.Bus.Publish(ev)
	}
	if ev, err := stream.Recv(); err != nil || ev.Type != "twitch.follow" || !strings.Contains(string(ev.DataJson), "fox") {
		t.Fatalf("event: %+v %v", ev, err)
	}

	if _, err := c.ChatSend(ctx, &pb.ChatSendRequest{Text: "/ban everyone"}); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 || !strings.HasPrefix((*sent)[0], "​/ban") {
		t.Fatalf("dev chat should go to the simulator, escaped: %q", *sent)
	}

	if n, err := c.StoreIncr(ctx, &pb.StoreIncrRequest{Key: "deaths", By: 2}); err != nil || n.Value != 2 {
		t.Fatalf("incr: %v %v", n, err)
	}
	if _, err := c.StoreSet(ctx, &pb.StoreSetRequest{Key: "x", ValueJson: []byte("not json")}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("non-JSON value: %v", err)
	}
	c.Log(ctx, &pb.LogRequest{Level: "warn", Msg: "careful"})
	if !strings.Contains(logs.String(), "[warn] careful") {
		t.Fatalf("log: %q", logs.String())
	}

	sub := h.Bus.Subscribe(4, "plugin.dev.test.hrc.spike")
	if _, err := c.Emit(ctx, &pb.EmitRequest{Name: "spike", DataJson: []byte(`{"bpm":160}`)}); err != nil {
		t.Fatal(err)
	}
	if ev := <-sub.C; ev.Source != "plugin:dev.test.hrc" || string(ev.Data) != `{"bpm":160}` {
		t.Fatalf("emit: %+v", ev)
	}

	if _, err := h.Invoke(ctx, "dev.test.hrc", &pb.Command{Kind: "cli", Name: "threshold"}); err != ErrNotRunning {
		t.Fatalf("invoke without a Commands stream: %v", err)
	}
	cmds, err := c.Commands(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		cmd, err := cmds.Recv()
		if err == nil {
			cmds.Send(&pb.CommandResult{Id: cmd.Id, DataJson: []byte(`{"threshold":` + string(cmd.ArgsJson[7:10]) + `}`)})
		}
	}()
	time.Sleep(100 * time.Millisecond)
	r, err := h.Invoke(ctx, "dev.test.hrc", &pb.Command{Kind: "cli", Name: "threshold", ArgsJson: []byte(`{"bpm":160}`)})
	if err != nil || string(r.DataJson) != `{"threshold":160}` {
		t.Fatalf("invoke: %+v %v", r, err)
	}
}

func TestNoPermissions(t *testing.T) {
	_, c, _, _ := setup(t, "id = \"dev.test.hrc\"\nversion = \"0.1.0\"\n[capsule]\nruntime = \"native\"\nentry = \"x\"\n")
	ctx := context.Background()
	if _, err := c.ChatSend(ctx, &pb.ChatSendRequest{Text: "hi"}); status.Code(err) != codes.PermissionDenied ||
		!strings.Contains(err.Error(), `add "send" to [permissions].chat`) {
		t.Fatalf("chat: %v", err)
	}
	if _, err := c.StoreGet(ctx, &pb.StoreKey{Key: "k"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("store: %v", err)
	}
}

func TestManifestRefusals(t *testing.T) {
	for _, bad := range []string{
		"[permissions]\nosc = [\"send\"]\n",
		"[permissions]\nmod = [\"nuke\"]\n",
		"[cli]\nname = \"capsule\"\n",
		"[[chat.commands]]\nname = \"hr\"\ncapability = \"made.up\"\n",
		"[config]\nx = { type = \"int\", default = \"nope\" }\n",
		"surprise = 1\n",
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "plugin.toml"), []byte("id = \"dev.a.b\"\napi = \"1\"\n"+bad), 0o644)
		if _, err := Load(dir); err == nil {
			t.Errorf("accepted:\n%s", bad)
		}
	}
}

func TestModTwitchViewers(t *testing.T) {
	_, c, logs, sent := setup(t, testPlugin) // chat=send only, no mod/twitch/viewers
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for name, err := range map[string]error{
		"mod": func() error {
			_, err := c.Mod(ctx, &pb.ModRequest{Action: "timeout", UserId: "1", Seconds: 60})
			return err
		}(),
		"twitch":   func() error { _, err := c.Twitch(ctx, &pb.TwitchRequest{Call: "stream"}); return err }(),
		"viewer":   func() error { _, err := c.Viewer(ctx, &pb.ViewerRequest{User: "1"}); return err }(),
		"announce": func() error { _, err := c.ChatSend(ctx, &pb.ChatSendRequest{Text: "hi", Mode: "announce"}); return err }(),
	} {
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s without permission: %v", name, err)
		}
	}

	_, c, logs, sent = setup(t, strings.Replace(testPlugin, `chat = ["send"]`,
		"chat = [\"send\", \"announce\"]\nmod = [\"timeout\", \"delete\"]\ntwitch = [\"read\"]\nviewers = true", 1))
	if _, err := c.Mod(ctx, &pb.ModRequest{Action: "timeout", UserId: "1", Seconds: 60, Reason: "caps"}); err != nil {
		t.Fatalf("dev-mode timeout should print, not fail: %v", err)
	}
	if !strings.Contains(logs.String(), "mod timeout 1 for 60s (caps)") {
		t.Fatalf("mod action not logged: %q", logs.String())
	}
	if _, err := c.Mod(ctx, &pb.ModRequest{Action: "unban", UserId: "1"}); err != nil {
		t.Fatalf("lifting a timeout is part of timeout: %v", err)
	}
	if _, err := c.Mod(ctx, &pb.ModRequest{Action: "ban", UserId: "1"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ban with only timeout: %v", err)
	}
	if _, err := c.Mod(ctx, &pb.ModRequest{Action: "timeout", UserId: "1", Seconds: 0}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("zero timeout: %v", err)
	}
	if _, err := c.Twitch(ctx, &pb.TwitchRequest{Call: "stream"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("twitch offline: %v", err)
	}
	if _, err := c.Twitch(ctx, &pb.TwitchRequest{Call: "clip"}); status.Code(err) != codes.PermissionDenied ||
		!strings.Contains(err.Error(), `"clips" to [permissions].twitch`) {
		t.Fatalf("clip with only read: %v", err)
	}
	if _, err := c.Twitch(ctx, &pb.TwitchRequest{Call: "drop_tables"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown call: %v", err)
	}
	if _, err := c.ChatSend(ctx, &pb.ChatSendRequest{Text: "big news", Mode: "announce"}); err != nil || (*sent)[len(*sent)-1] != "big news" {
		t.Fatalf("announce: %v %q", err, *sent)
	}
	if r, err := c.Viewer(ctx, &pb.ViewerRequest{User: "nobody"}); err != nil || string(r.DataJson) != "null" {
		t.Fatalf("unknown viewer: %s %v", r.GetDataJson(), err)
	}
	if _, err := c.StoreBatch(ctx, &pb.StoreBatchRequest{Set: []*pb.StoreSetRequest{{Key: "a", ValueJson: []byte("1")}}, Delete: []string{"b"}}); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.StoreGet(ctx, &pb.StoreKey{Key: "a"}); string(v.ValueJson) != "1" {
		t.Fatalf("batch set: %s", v.ValueJson)
	}
}
