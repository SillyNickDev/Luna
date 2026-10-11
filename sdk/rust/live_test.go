package rustsdk

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"luna/internal/bus"
	"luna/internal/capsule"
	"luna/internal/capsule/sandbox"
	"luna/internal/config"
	"luna/internal/event"
	"luna/internal/guard"
	"luna/internal/plugin"
	"luna/internal/plugin/pb"
	"luna/internal/store"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == sandbox.InitArg {
		sandbox.Init()
	}
	os.Exit(m.Run())
}

// The Rust SDK's live path against Luna's real plugin server: the control channel's
// CBOR, gRPC over HTTP/2 and the protobuf tables. Needs cargo on PATH.
func TestLive(t *testing.T) {
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("no cargo on PATH")
	}
	if out, err := exec.Command("cargo", "build", "-q", "--example", "live").CombinedOutput(); err != nil {
		t.Fatalf("cargo build: %v\n%s", err, out)
	}
	dir, _ := filepath.Abs("testdata/live")
	m, err := plugin.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "luna.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sent := make(chan string, 4)
	g := guard.New(config.Default)
	g.Live = func(_ context.Context, msg guard.Msg) error { sent <- msg.Text; return nil }
	b := bus.New()
	host := &plugin.Host{Bus: b, Publish: b.Publish, Store: st, Guard: g}
	cfg, err := m.EffectiveConfig(map[string]any{"greeting": "Hey"})
	if err != nil {
		t.Fatal(err)
	}

	sock, _ := os.MkdirTemp("", "lrust") // short: Unix socket paths are limited
	defer os.RemoveAll(sock)
	hostLn, err := net.Listen("unix", filepath.Join(sock, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer hostLn.Close()
	go host.Serve(plugin.Plugin{ID: m.ID, Manifest: m, Config: cfg, Log: testLog{t}}, hostLn)
	ctlLn, err := net.Listen("unix", filepath.Join(sock, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ctlLn.Close()

	cmd := exec.Command("target/debug/examples/live")
	cmd.Env = append(os.Environ(), "CAPSULE_CONTROL="+filepath.Join(sock, "control.sock"),
		"CAPSULE_HOST="+filepath.Join(sock, "host.sock"), "CAPSULE_LINKS="+filepath.Join(sock, "links"))
	cmd.Stdout, cmd.Stderr = testLog{t}, testLog{t}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	// Play the runtime's side of the control channel.
	ctlLn.(*net.UnixListener).SetDeadline(time.Now().Add(15 * time.Second))
	c, err := ctlLn.Accept()
	if err != nil {
		t.Fatal(err)
	}
	ctl := capsule.NewConn(c)
	if f, err := ctl.Read(); err != nil || f.T != "hello" {
		t.Fatalf("hello: %+v %v", f, err)
	}
	ctl.Write("welcome", 0, capsule.Welcome{ID: m.ID})
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	if f, err := ctl.Read(); err != nil || f.T != "ready" {
		t.Fatalf("ready: %+v %v", f, err)
	}
	ctl.Write("ping", 7, nil)
	if f, err := ctl.Read(); err != nil || f.T != "pong" || f.ID != 7 {
		t.Fatalf("pong: %+v %v", f, err)
	}

	if v, _ := st.PluginGet(m.ID, "started"); string(v) != `{"at":1,"big":1099511627776,"neg":-5}` {
		t.Fatalf("onStart store write: %s", v)
	}
	ev, _ := event.New("twitch.channel.follow", "twitch", false, map[string]any{"user": map[string]any{"id": "1", "login": "fox", "name": "Fox"}})
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		b.Publish(ev) // until the plugin's subscription is in place
		select {
		case got := <-sent:
			if got != "Hey, Fox! #1" {
				t.Fatalf("chat: %q", got)
			}
		default:
			if time.Now().After(deadline) {
				t.Fatal("no chat message for the follow")
			}
			continue
		}
		break
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var r *pb.CommandResult
	for r == nil || r.Error != "" {
		if r, err = host.Invoke(ctx, m.ID, &pb.Command{Kind: "cli", Name: "count"}); err != nil && ctx.Err() != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var out map[string]int
	if json.Unmarshal(r.DataJson, &out); out["follows"] < 1 {
		t.Fatalf("cli: %s", r.DataJson)
	}
	ctl.Write("call", 9, capsule.LinkCall{Link: "echo", Call: capsule.Call{Method: "echo", Body: []byte{0x02}, Caller: "dev.test.other"}})
	f, err := ctl.Read()
	if err != nil || f.T != "result" || f.ID != 9 {
		t.Fatalf("link result: %+v %v", f, err)
	}
	var res capsule.Result
	var body map[string]any
	if capsule.Decode(f.B, &res); !res.OK || capsule.Decode(res.Body, &body) != nil || body["caller"] != "dev.test.other" || body["body"] != uint64(2) {
		t.Fatalf("link result: %+v %v", res, body)
	}
	ctl.Write("shutdown", 0, capsule.Shutdown{DeadlineMS: 2000})
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exit after shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still running after shutdown")
	}
}

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// luna capsule dev asks a native plugin to describe itself, in the sandbox, with no SDK mounted.
func TestDescribeNative(t *testing.T) {
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("no cargo on PATH")
	}
	cmd := exec.Command("cargo", "build", "-q", "--release", "--example", "live", "--target", "x86_64-unknown-linux-gnu")
	cmd.Env = append(os.Environ(), "RUSTFLAGS=-C target-feature=+crt-static")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cargo build: %v\n%s", err, out)
	}
	dir := t.TempDir()
	toml, _ := os.ReadFile("testdata/live/plugin.toml")
	bin, _ := os.ReadFile("target/x86_64-unknown-linux-gnu/release/examples/live")
	os.MkdirAll(filepath.Join(dir, "bin/linux-amd64"), 0o755)
	os.WriteFile(filepath.Join(dir, "plugin.toml"), toml, 0o644)
	os.WriteFile(filepath.Join(dir, "bin/linux-amd64/live"), bin, 0o755)
	run, _ := os.MkdirTemp("", "lrd")
	defer os.RemoveAll(run)
	out, err := capsule.Describe(dir, run, run, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Events []string
		Links  struct{ Provides []string }
	}
	if json.Unmarshal(out, &d); len(d.Events) != 1 || len(d.Links.Provides) != 1 {
		t.Fatalf("describe: %s", out)
	}
}
