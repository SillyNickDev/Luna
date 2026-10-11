//go:build windows

package sandbox

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

// The Windows half of the conformance suite (CONFORMANCE.md). Run on Windows 10 1809+:
// go test ./internal/capsule/sandbox/

func buildProbe(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "probe.exe"), "./testdata/probe")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building probe: %v\n%s", err, out)
	}
}

type box struct{ pkg, data, secret string }

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
		Mounts: []Mount{{Src: b.pkg, Dst: "/pkg"}, {Src: b.data, Dst: "/data", RW: true}},
		Argv:   append([]string{"/pkg/probe.exe"}, args...),
		Env:    []string{"CAPSULE_TEST=1", "HOME=/data"},
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
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\LunaCapsuleTest`, registry.ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	k.SetStringValue("secret", "oauth:secret")
	k.Close()
	t.Cleanup(func() { registry.DeleteKey(registry.CURRENT_USER, `Software\LunaCapsuleTest`) })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	sockDir, _ := os.MkdirTemp("", "lk")
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	other, err := net.Listen("unix", filepath.Join(sockDir, "dev.other-show"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	cases := []struct {
		args []string
		ok   bool
	}{
		{[]string{"read", "/pkg/probe.exe"}, true},
		{[]string{"write", "/data/x"}, true},
		{[]string{"read", b.secret}, false},
		{[]string{"ls", home}, false},
		{[]string{"read", filepath.Join(home, "NTUSER.DAT")}, false},
		{[]string{"write", "/pkg/x"}, false},
		{[]string{"exec", "/data/x"}, false},
		{[]string{"tcp", "1.1.1.1:443"}, false},
		{[]string{"tcp", ln.Addr().String()}, false}, // AppContainers can't reach loopback either
		{[]string{"kill", strconv.Itoa(os.Getpid())}, false},
		{[]string{"registry", "LunaCapsuleTest"}, false},
		{[]string{"unix", filepath.Join(sockDir, "dev.other-show")}, false},
	}
	for _, c := range cases {
		out, ex := b.run(t, 64<<20, c.args...)
		if got := out == "OK"; got != c.ok {
			t.Errorf("%v: got %q (exit %+v), want ok=%v", c.args, out, ex, c.ok)
		}
	}
	out, _ := b.run(t, 64<<20, "env")
	if !strings.Contains(out, "CAPSULE_TEST=1") || !strings.Contains(out, "HOME="+b.data) || strings.Contains(out, "USERPROFILE=") {
		t.Errorf("environment leaked, missing or not rewritten: %s", out)
	}
}

func TestLimits(t *testing.T) {
	b := newBox(t)
	if out, ex := b.run(t, 32<<20, "alloc", "100"); ex.Cause != "oom" {
		t.Errorf("alloc past the cap: got %q %+v, want oom", out, ex)
	}
	if out, _ := b.run(t, 64<<20, "fork", "3"); !strings.HasPrefix(out, "DENIED") {
		t.Errorf("starting a child process: got %q", out)
	}
	if out, ex := b.run(t, 64<<20, "exit", "3"); ex.Cause != "exit" || ex.Code != 3 {
		t.Errorf("exit code: got %q %+v", out, ex)
	}
}

// Stopping a capsule ends it even mid-sleep (the job's kill), and reports it.
func TestKill(t *testing.T) {
	b := newBox(t)
	var out bytes.Buffer
	p, err := Start(Spec{Name: "test", Mounts: []Mount{{Src: b.pkg, Dst: "/pkg"}}, Argv: []string{"/pkg/probe.exe", "sleep"},
		Dir: "/pkg", Memory: 64 << 20, CPU: 0.5, Pids: 8, Stdout: &out, Stderr: &out})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	p.Kill()
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("still running after Kill")
	}
	if ex := p.Exit(); ex.Cause != "exit" || ex.Code != 1 {
		t.Errorf("killed capsule: %+v", ex)
	}
}

// A single file can be mounted (the luna runtime mounts luna.exe), read-only, without the folder around it.
func TestFileMount(t *testing.T) {
	b := newBox(t)
	os.WriteFile(filepath.Join(b.pkg, "beside.txt"), []byte("x"), 0o644)
	run := func(args ...string) string {
		var out bytes.Buffer
		p, err := Start(Spec{Name: "test-file", Mounts: []Mount{{Src: filepath.Join(b.pkg, "probe.exe"), Dst: "/tool"}},
			Argv: append([]string{"/tool"}, args...), Dir: b.data, Memory: 64 << 20, CPU: 0.5, Pids: 8, Stdout: &out, Stderr: &out})
		if err != nil {
			t.Fatal(err)
		}
		<-p.Done()
		return strings.TrimSpace(out.String())
	}
	if out := run("read", "/tool"); out != "OK" {
		t.Fatalf("running and reading a mounted file: %s", out)
	}
	if out := run("write", "/tool"); out == "OK" {
		t.Fatal("a mounted file is writable")
	}
	if out := run("read", filepath.Join(b.pkg, "beside.txt")); out == "OK" {
		t.Fatal("the folder around a mounted file is readable")
	}
}
