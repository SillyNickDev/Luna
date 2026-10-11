//go:build linux

package capsule

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"luna/internal/capsule/sandbox"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == sandbox.InitArg {
		sandbox.Init()
	}
	os.Exit(m.Run())
}

var (
	echoOnce sync.Once
	echoBin  string
)

// echoPkg builds testdata/echo into a verified package running `echo <mode>`.
func echoPkg(t *testing.T, store, id, mode, extra string) *Unpacked {
	t.Helper()
	echoOnce.Do(func() {
		echoBin = filepath.Join(os.TempDir(), fmt.Sprintf("luna-echo-%d", os.Getpid()))
		cmd := exec.Command("go", "build", "-o", echoBin, "./testdata/echo")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building echo: %v\n%s", err, out)
		}
	})
	bin, _ := os.ReadFile(echoBin)
	dir := writePkg(t, map[string]string{
		"capsule.toml": fmt.Sprintf("spec = \"0.1\"\nid = %q\nversion = \"0.1.0\"\nruntime = \"native\"\nentry = \"echo\"\nargs = [%q]\nrestart = \"on-failure\"\nhealth = { interval = \"1s\", timeout = \"300ms\" }\n%s", id, mode, extra),
		"echo":         string(bin),
	})
	if err := WriteLock(dir); err != nil {
		t.Fatal(err)
	}
	u, err := AddToStore(store, dir)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

type states struct {
	mu  sync.Mutex
	log []string
}

func (s *states) add(id, state string, f *Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := id + " " + state
	if f != nil {
		e += " " + f.Cause
	}
	s.log = append(s.log, e)
}

func (s *states) wait(t *testing.T, want string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		s.mu.Lock()
		got := strings.Join(s.log, "\n")
		s.mu.Unlock()
		if strings.Contains(got, want) {
			return
		}
	}
	t.Fatalf("never saw %q; states:\n%s", want, strings.Join(s.log, "\n"))
}

func newRuntime(t *testing.T) (*Runtime, *states, string) {
	run, err := os.MkdirTemp("", "lr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	s := &states{}
	rt := &Runtime{RunDir: run, DataDir: t.TempDir(), OnState: s.add}
	t.Cleanup(rt.StopAll)
	return rt, s, t.TempDir()
}

func TestLifecycle(t *testing.T) {
	rt, s, store := newRuntime(t)
	c, err := rt.Enable(echoPkg(t, store, "dev.test.ok", "ok", ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.wait(t, "dev.test.ok running")
	time.Sleep(2500 * time.Millisecond) // a couple of health checks pass
	if c.State() != Running {
		t.Fatalf("state %s after health checks; logs: %v", c.State(), c.Logs.Tail(20))
	}
	c.Stop()
	if c.State() != Stopped || len(c.History()) != 0 {
		t.Fatalf("state %s, failures %+v", c.State(), c.History())
	}
	if logs := strings.Join(c.Logs.Tail(10), "\n"); !strings.Contains(logs, "[info] hello from ok") || !strings.Contains(logs, "shutting down") {
		t.Fatalf("log frame or graceful shutdown missing:\n%s", logs)
	}
}

func TestRestartsThenFails(t *testing.T) {
	backoffBase = 10 * time.Millisecond
	t.Cleanup(func() { backoffBase = time.Second }) // after StopAll, which is registered later
	rt, s, store := newRuntime(t)
	c, _ := rt.Enable(echoPkg(t, store, "dev.test.exit", "exit", ""), nil)
	s.wait(t, "dev.test.exit failed exit")
	if h := c.History(); len(h) != maxFailures || h[0].Code != 3 {
		t.Fatalf("history: %+v", h)
	}
	c.Start() // luna capsule restart clears failed
	s.wait(t, "dev.test.exit running")
}

func TestHandshakeAndHealth(t *testing.T) {
	handshakeTimeout = 500 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = 10 * time.Second })
	rt, s, store := newRuntime(t)
	rt.Enable(echoPkg(t, store, "dev.test.nohello", "nohello", ""), nil)
	s.wait(t, "dev.test.nohello restarting handshake")
	rt.Enable(echoPkg(t, store, "dev.test.nopong", "nopong", ""), nil)
	s.wait(t, "dev.test.nopong restarting unhealthy")
}

func TestLinks(t *testing.T) {
	rt, s, store := newRuntime(t)
	rt.Enable(echoPkg(t, store, "dev.test.provider", "ok", "[links]\nprovides = [\"echo\"]\n"), nil)
	s.wait(t, "dev.test.provider running")
	use := "dev.test.provider:echo"
	c, err := rt.Enable(echoPkg(t, store, "dev.test.consumer", "consumer", "[links]\nuses = [\""+use+"\"]\n"), []string{use})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		logs := strings.Join(c.Logs.Tail(20), "\n")
		if strings.Contains(logs, "big frame refused") {
			if !strings.Contains(logs, "result: true echo/say from dev.test.consumer") {
				t.Fatalf("caller not stamped by the broker:\n%s", logs)
			}
			return
		}
	}
	t.Fatalf("no link result; logs:\n%s", strings.Join(c.Logs.Tail(20), "\n"))
}
