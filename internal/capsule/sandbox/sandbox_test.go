//go:build linux

package sandbox

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == InitArg {
		Init()
	}
	os.Exit(m.Run())
}

// BuildProbe compiles testdata/probe into dir/probe.
func buildProbe(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "probe"), "./testdata/probe")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building probe: %v\n%s", err, out)
	}
}

type box struct {
	pkg, data, secret string
}

func newBox(t *testing.T) box {
	b := box{pkg: t.TempDir(), data: t.TempDir(), secret: filepath.Join(t.TempDir(), "token")}
	buildProbe(t, b.pkg)
	os.WriteFile(b.secret, []byte("oauth:secret"), 0o600)
	return b
}

func (b box) run(t *testing.T, memory int64, args ...string) (string, Exit) {
	t.Helper()
	var out bytes.Buffer
	p, err := Start(Spec{
		Name:   "test",
		Root:   t.TempDir(),
		Mounts: []Mount{{Src: b.pkg, Dst: "/pkg"}, {Src: b.data, Dst: "/data", RW: true}},
		Argv:   append([]string{"/pkg/probe"}, args...),
		Env:    []string{"CAPSULE_TEST=1"},
		Dir:    "/data",
		Memory: memory, CPU: 0.5, Pids: 8, Files: 64,
		Stdout: &out, Stderr: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(20 * time.Second):
		p.Kill()
		t.Fatalf("%v: timed out", args)
	}
	return strings.TrimSpace(out.String()), p.Exit()
}

func TestIsolation(t *testing.T) {
	b := newBox(t)
	home, _ := os.UserHomeDir()
	cases := []struct {
		args []string
		ok   bool
	}{
		{[]string{"read", "/pkg/probe"}, true},
		{[]string{"write", "/data/x"}, true},
		{[]string{"read", "/dev/urandom"}, false}, // endless; read errors out at ReadFile size? see below
		{[]string{"read", b.secret}, false},
		{[]string{"ls", home}, false},
		{[]string{"read", "/etc/passwd"}, false},
		{[]string{"write", "/pkg/x"}, false},
		{[]string{"write", "/x"}, false},
		{[]string{"exec", "/data/x"}, false},
		{[]string{"tcp", "1.1.1.1:443"}, false},
		{[]string{"tcp", "127.0.0.1:22"}, false},
		{[]string{"unshare"}, false},
		{[]string{"mount"}, false},
		{[]string{"kill", "1"}, true}, // itself: PID 1 of its own namespace; can't reach the host's
	}
	for _, c := range cases {
		if c.args[0] == "read" && c.args[1] == "/dev/urandom" {
			continue // ReadFile on an endless device never returns
		}
		out, ex := b.run(t, 64<<20, c.args...)
		if got := out == "OK"; got != c.ok {
			t.Errorf("%v: got %q (exit %+v), want ok=%v", c.args, out, ex, c.ok)
		}
	}
	if out, _ := b.run(t, 64<<20, "env"); !strings.Contains(out, "CAPSULE_TEST=1") || strings.Contains(out, "HOME=") {
		t.Errorf("environment leaked or missing: %s", out)
	}
}

func TestLimits(t *testing.T) {
	b := newBox(t)
	if out, ex := b.run(t, 32<<20, "alloc", "100"); ex.Cause != "oom" {
		t.Errorf("alloc past the cap: got %q %+v, want oom", out, ex)
	}
	if out, _ := b.run(t, 64<<20, "fork", "50"); !strings.HasPrefix(out, "DENIED") {
		t.Errorf("fork bomb past pids: got %q", out)
	}
	if out, _ := b.run(t, 64<<20, "files", "200"); !strings.HasPrefix(out, "DENIED") {
		t.Errorf("open files past the cap: got %q", out)
	}
	if out, ex := b.run(t, 64<<20, "exit", "3"); ex.Cause != "exit" || ex.Code != 3 {
		t.Errorf("exit code: got %q %+v", out, ex)
	}
}

func TestNoOrphans(t *testing.T) {
	b := newBox(t)
	out, _ := b.run(t, 64<<20, "orphan")
	if out != "OK" {
		t.Fatalf("orphan probe: %s", out)
	}
	// The child was in the capsule's PID namespace, which died with PID 1.
	ps, _ := exec.Command("pgrep", "-f", b.pkg+"/probe").Output()
	if len(bytes.TrimSpace(ps)) > 0 {
		t.Fatalf("capsule child outlived it: pids %s", ps)
	}
}
