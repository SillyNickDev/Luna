package capsule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"luna/internal/capsule/sandbox"
)

// Lifecycle states, reported to the host as bot.plugin.<state>.
const (
	Stopped    = "stopped"
	Starting   = "starting"
	Running    = "running"
	Stopping   = "stopping"
	Restarting = "restarting"
	Failed     = "failed"
)

// Vars so tests can shorten them.
var (
	handshakeTimeout = 10 * time.Second
	backoffBase      = time.Second
)

const (
	shutdownDeadline = 5 * time.Second
	failWindow       = 10 * time.Minute
	maxFailures      = 5
	maxBackoff       = 60 * time.Second
)

// Failure is why a capsule died, with the end of its log.
type Failure struct {
	Time   time.Time
	Cause  string // exit, crash, oom, unhealthy, handshake, policy
	Code   int
	Signal string
	Logs   []string
}

// Runtime supervises every enabled capsule on this host.
type Runtime struct {
	RunDir  string // sockets and capsule roots; keep it short, Unix socket paths max out at 108 bytes
	DataDir string // one persistent data/ folder per capsule ID
	// OnState is called on every state change; Luna publishes it as bot.plugin.<state>.
	OnState func(id, state string, f *Failure)
	// OnHost receives connections on a capsule's host socket (the plugin API).
	OnHost func(id string, c net.Conn)

	mu   sync.Mutex
	caps map[string]*Capsule
}

// Capsule is one installed package and its supervisor.
type Capsule struct {
	rt   *Runtime
	ID   string
	Pkg  *Unpacked
	Uses []string // links.uses the user approved
	Logs *Ring

	mu       sync.Mutex
	state    string
	failures []time.Time
	history  []Failure
	started  time.Time
	stop     context.CancelFunc
	stopped  chan struct{}
	ctl      *Conn
	calls    map[uint64]chan Result
	nextCall uint64
}

func (rt *Runtime) Get(id string) *Capsule {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.caps[id]
}

func (rt *Runtime) List() []*Capsule {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]*Capsule, 0, len(rt.caps))
	for _, c := range rt.caps {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b *Capsule) int { return cmpStr(a.ID, b.ID) })
	return out
}

func cmpStr(a, b string) int {
	if a < b {
		return -1
	} else if a > b {
		return 1
	}
	return 0
}

// Enable registers pkg and starts supervising it, replacing any running version.
// shortcut: capsules start when enabled, not lazily on their first event; lazy start
// and 15-minute hibernation need the Phase 3 host API's event subscriptions to define "idle".
func (rt *Runtime) Enable(pkg *Unpacked, uses []string) (*Capsule, error) {
	if err := CanRun(pkg); err != nil {
		return nil, err
	}
	m := pkg.Manifest
	rt.Disable(m.ID)
	c := &Capsule{rt: rt, ID: m.ID, Pkg: pkg, Uses: uses, Logs: NewRing(1 << 20), state: Stopped}
	rt.mu.Lock()
	if rt.caps == nil {
		rt.caps = map[string]*Capsule{}
	}
	rt.caps[c.ID] = c
	rt.mu.Unlock()
	c.Start()
	return c, nil
}

// CanRun says why this host can't run pkg, failing closed on features not built yet.
func CanRun(pkg *Unpacked) error {
	m := pkg.Manifest
	if len(m.Net) > 0 {
		return errors.New("network allowlists aren't supported yet; this package can't run")
	}
	if len(m.Devices) > 0 {
		return errors.New("device access isn't supported yet; this package can't run")
	}
	if m.Runtime != "native" {
		return fmt.Errorf("runtime_missing: %s isn't available yet; only native capsules run for now", m.Runtime)
	}
	_, err := entryPath(pkg)
	return err
}

// Disable stops a capsule and forgets it.
func (rt *Runtime) Disable(id string) {
	if c := rt.Get(id); c != nil {
		c.Stop()
		rt.mu.Lock()
		delete(rt.caps, id)
		rt.mu.Unlock()
	}
}

// StopAll shuts every capsule down, in parallel.
func (rt *Runtime) StopAll() {
	var wg sync.WaitGroup
	for _, c := range rt.List() {
		wg.Go(c.Stop)
	}
	wg.Wait()
}

func (c *Capsule) State() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// History returns past failures, oldest first.
func (c *Capsule) History() []Failure {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.history)
}

func (c *Capsule) setState(s string, f *Failure) {
	c.mu.Lock()
	changed := c.state != s
	c.state = s
	c.mu.Unlock()
	if changed && c.rt.OnState != nil {
		c.rt.OnState(c.ID, s, f)
	}
}

// Start begins supervising, and also clears a failed state (luna capsule restart).
func (c *Capsule) Start() {
	c.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.stop, c.stopped, c.failures = cancel, make(chan struct{}), nil
	c.mu.Unlock()
	go c.supervise(ctx)
}

// Stop shuts the capsule down gracefully and waits until its whole tree is gone.
func (c *Capsule) Stop() {
	c.mu.Lock()
	stop, stopped := c.stop, c.stopped
	c.stop = nil
	c.mu.Unlock()
	if stop != nil {
		stop()
		<-stopped
	}
}

func (c *Capsule) supervise(ctx context.Context) {
	defer close(c.stopped)
	backoff := backoffBase
	for {
		f := c.runOnce(ctx)
		if ctx.Err() != nil {
			c.setState(Stopped, nil)
			return
		}
		m := c.Pkg.Manifest
		c.mu.Lock()
		if f != nil {
			c.history = append(c.history, *f)
			if len(c.history) > 20 {
				c.history = c.history[1:]
			}
			now := time.Now()
			c.failures = slices.DeleteFunc(append(c.failures, now), func(t time.Time) bool { return now.Sub(t) > failWindow })
		}
		if time.Since(c.started) >= failWindow {
			backoff = backoffBase // ran long enough; forgive the past
		}
		tooMany := len(c.failures) >= maxFailures
		c.mu.Unlock()
		switch {
		case f != nil:
			slog.Warn("capsule died", "capsule", c.ID, "cause", f.Cause, "code", f.Code, "signal", f.Signal)
		}
		switch {
		case tooMany:
			c.setState(Failed, f)
			return
		case m.Restart == "never", m.Restart == "on-failure" && f == nil:
			c.setState(Stopped, f)
			return
		}
		c.setState(Restarting, f)
		select {
		case <-ctx.Done():
			c.setState(Stopped, nil)
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// runOnce starts the capsule, supervises it until it exits or ctx ends, and returns
// the failure (nil for a clean exit or a requested stop).
func (c *Capsule) runOnce(ctx context.Context) *Failure {
	c.setState(Starting, nil)
	c.mu.Lock()
	c.started = time.Now()
	c.mu.Unlock()
	base := filepath.Join(c.rt.RunDir, shortID(c.ID))
	os.RemoveAll(base)
	defer os.RemoveAll(base)
	sock := filepath.Join(base, "sock")
	if err := os.MkdirAll(filepath.Join(sock, "links"), 0o700); err != nil {
		return c.fail("handshake", err)
	}
	if err := os.Mkdir(filepath.Join(base, "root"), 0o700); err != nil {
		return c.fail("handshake", err)
	}
	ctlLn, err := listenUnix(filepath.Join(sock, "control.sock"))
	if err != nil {
		return c.fail("handshake", err)
	}
	defer ctlLn.Close()
	var lns []net.Listener
	defer func() {
		for _, ln := range lns {
			ln.Close()
		}
	}()
	hostLn, err := listenUnix(filepath.Join(sock, "host.sock"))
	if err != nil {
		return c.fail("handshake", err)
	}
	lns = append(lns, hostLn)
	go c.acceptHost(hostLn)
	for _, use := range c.Uses {
		ln, err := listenUnix(filepath.Join(sock, "links", use))
		if err != nil {
			return c.fail("handshake", err)
		}
		lns = append(lns, ln)
		go c.acceptLink(ln, use)
	}

	m := c.Pkg.Manifest
	entry, _ := entryPath(c.Pkg)
	mounts := []sandbox.Mount{{Src: c.Pkg.Dir, Dst: "/pkg"}, {Src: sock, Dst: "/run/capsule"}}
	dir := "/pkg"
	if m.FS.Writable == "data" {
		data := filepath.Join(c.rt.DataDir, c.ID)
		if err := os.MkdirAll(data, 0o700); err != nil {
			return c.fail("handshake", err)
		}
		mounts = append(mounts, sandbox.Mount{Src: data, Dst: "/data", RW: true})
		dir = "/data"
	}
	env := []string{
		"CAPSULE_ID=" + c.ID,
		"CAPSULE_CONTROL=/run/capsule/control.sock",
		"CAPSULE_HOST=/run/capsule/host.sock",
		"CAPSULE_LINKS=/run/capsule/links",
		"HOME=" + dir,
	}
	for k, v := range m.Env {
		env = append(env, k+"="+v)
	}
	proc, err := sandbox.Start(sandbox.Spec{
		Name:   c.ID,
		Root:   filepath.Join(base, "root"),
		Mounts: mounts,
		Argv:   append([]string{entry}, m.Args...),
		Env:    env,
		Dir:    dir,
		Memory: int64(m.Limits.Memory), CPU: m.Limits.CPU, Pids: m.Limits.Pids, Files: m.Limits.Files,
		FileSize: int64(m.FS.DataQuota),
		Stdout:   c.Logs, Stderr: c.Logs,
	})
	if err != nil {
		return c.fail("handshake", err)
	}
	cause := ""
	kill := func(why string) {
		if cause == "" {
			cause = why
		}
		proc.Kill()
	}
	defer func() {
		proc.Kill() // the tree never outlives this function
		<-proc.Done()
	}()

	ctl, err := c.handshake(ctx, ctlLn, proc)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		kill("handshake")
		<-proc.Done()
		return c.exitFailure(proc.Exit(), cause, err)
	}
	ctlLn.Close()
	c.mu.Lock()
	c.ctl, c.calls = ctl, map[uint64]chan Result{}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.ctl = nil
		for _, ch := range c.calls {
			close(ch)
		}
		c.calls = nil
		c.mu.Unlock()
		c.rt.linkEvent(c, "link_down")
	}()
	c.setState(Running, nil)
	c.rt.linkEvent(c, "link_up")
	c.rt.linksUp(c, ctl)

	pongs := make(chan uint64, 1)
	go c.readControl(ctl, pongs)
	interval, timeout := time.Duration(m.Health.Interval), time.Duration(m.Health.Timeout)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	var ping uint64
	missed := 0
	for {
		select {
		case <-proc.Done():
			return c.exitFailure(proc.Exit(), cause, nil)
		case <-ctx.Done():
			c.setState(Stopping, nil)
			ctl.Write("shutdown", 0, Shutdown{DeadlineMS: shutdownDeadline.Milliseconds()})
			select {
			case <-proc.Done():
			case <-time.After(shutdownDeadline):
			}
			return nil
		case <-tick.C:
			ping++
			ctl.Write("ping", ping, nil)
			select {
			case id := <-pongs:
				if id == ping {
					missed = 0
					continue
				}
			case <-time.After(timeout):
			case <-proc.Done():
				return c.exitFailure(proc.Exit(), cause, nil)
			}
			if missed++; missed >= 2 {
				kill("unhealthy")
			}
		}
	}
}

// handshake accepts the control connection and runs hello → welcome → ready within 10 s.
func (c *Capsule) handshake(ctx context.Context, ln *net.UnixListener, proc *sandbox.Proc) (*Conn, error) {
	deadline := time.Now().Add(handshakeTimeout)
	ln.SetDeadline(deadline)
	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() { conn, err := ln.Accept(); ch <- accepted{conn, err} }()
	var a accepted
	select {
	case a = <-ch:
	case <-proc.Done():
		return nil, errors.New("exited before connecting to its control socket")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if a.err != nil {
		return nil, fmt.Errorf("no hello within %s", handshakeTimeout)
	}
	a.conn.SetDeadline(deadline)
	ctl := NewConn(a.conn)
	f, err := ctl.Read()
	if err != nil || f.T != "hello" {
		return nil, fmt.Errorf("expected hello first: %v", err)
	}
	var h Hello
	if err := Decode(f.B, &h); err != nil || h.Spec != SpecVersion {
		return nil, fmt.Errorf("unsupported spec version %q (want %q)", h.Spec, SpecVersion)
	}
	m := c.Pkg.Manifest
	err = ctl.Write("welcome", 0, Welcome{ID: c.ID, Links: c.Uses, Limits: map[string]any{
		"memory": int64(m.Limits.Memory), "cpu": m.Limits.CPU, "pids": m.Limits.Pids, "files": m.Limits.Files,
	}})
	if err != nil {
		return nil, err
	}
	for {
		f, err := ctl.Read()
		if err != nil {
			return nil, fmt.Errorf("no ready: %w", err)
		}
		switch f.T {
		case "ready":
			a.conn.SetDeadline(time.Time{})
			return ctl, nil
		case "log":
			c.logFrame(f)
		default:
			return nil, fmt.Errorf("unexpected %q before ready", f.T)
		}
	}
}

func (c *Capsule) readControl(ctl *Conn, pongs chan<- uint64) {
	for {
		f, err := ctl.Read()
		if err != nil {
			return // the process exit is what the supervisor acts on
		}
		switch f.T {
		case "pong":
			select {
			case pongs <- f.ID:
			default:
			}
		case "log":
			c.logFrame(f)
		case "result":
			var r Result
			if Decode(f.B, &r) != nil {
				continue
			}
			c.mu.Lock()
			ch := c.calls[f.ID]
			delete(c.calls, f.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- r
			}
		}
	}
}

func (c *Capsule) logFrame(f Frame) {
	var l Log
	if Decode(f.B, &l) == nil {
		fmt.Fprintf(c.Logs, "[%s] %s\n", l.Level, l.Msg)
	}
}

func (c *Capsule) acceptHost(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if c.rt.OnHost != nil {
			go c.rt.OnHost(c.ID, conn)
		} else {
			conn.Close()
		}
	}
}

func (c *Capsule) fail(cause string, err error) *Failure {
	fmt.Fprintf(c.Logs, "luna: %v\n", err)
	return &Failure{Time: time.Now(), Cause: cause, Logs: c.Logs.Tail(200)}
}

// exitFailure turns a process exit into a failure record; a clean exit is nil.
// A cause Luna chose (it killed the capsule) beats what the exit looks like.
func (c *Capsule) exitFailure(ex sandbox.Exit, cause string, err error) *Failure {
	if err != nil {
		fmt.Fprintf(c.Logs, "luna: %v\n", err)
	}
	if cause == "" {
		cause = ex.Cause
	}
	if cause == "" {
		return nil
	}
	return &Failure{Time: time.Now(), Cause: cause, Code: ex.Code, Signal: ex.Signal, Logs: c.Logs.Tail(200)}
}

// entryPath picks bin/<os>-<arch>/<entry> when the package ships per-platform builds.
func entryPath(pkg *Unpacked) (string, error) {
	m := pkg.Manifest
	plat := filepath.Join("bin", runtime.GOOS+"-"+runtime.GOARCH, m.Entry)
	if _, err := os.Stat(filepath.Join(pkg.Dir, plat)); err == nil {
		return "/pkg/" + filepath.ToSlash(plat), nil
	}
	if _, err := os.Stat(filepath.Join(pkg.Dir, "bin")); err == nil {
		return "", fmt.Errorf("no_platform_build: no bin/%s-%s/%s in the package", runtime.GOOS, runtime.GOARCH, m.Entry)
	}
	if _, err := os.Stat(filepath.Join(pkg.Dir, m.Entry)); err != nil {
		return "", fmt.Errorf("entry %s isn't in the package", m.Entry)
	}
	return "/pkg/" + m.Entry, nil
}

// shortID keeps socket paths short whatever the capsule ID's length.
func shortID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:6])
}

func listenUnix(path string) (*net.UnixListener, error) {
	if len(path) >= 108 {
		return nil, fmt.Errorf("socket path too long (%d bytes): %s", len(path), path)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	os.Chmod(path, 0o600)
	return ln, nil
}
