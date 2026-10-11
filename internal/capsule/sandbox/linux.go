//go:build linux

package sandbox

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

type initConfig struct {
	Spec
	Cgroup bool
}

// Proc is a running capsule. Its entry is PID 1 of a fresh PID namespace,
// so killing it kills the whole tree.
type Proc struct {
	cmd     *exec.Cmd
	cg      string // cgroup dir; "" means rlimits plus the RSS watchdog
	done    chan struct{}
	exit    Exit
	oomBase int64       // the parent slice's oom_kill count at start
	oom     atomic.Bool // the watchdog killed it
	killMu  sync.Mutex
}

func Start(s Spec) (*Proc, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer w.Close()
	cmd := exec.Command("/proc/self/exe", InitArg)
	cmd.Args[0] = "luna-capsule"
	cmd.Env = []string{}
	cmd.Stdout, cmd.Stderr = s.Stdout, s.Stderr
	cmd.ExtraFiles = []*os.File{r}
	uid, gid := os.Getuid(), os.Getgid()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNS |
			unix.CLONE_NEWNET | unix.CLONE_NEWIPC | unix.CLONE_NEWUTS | unix.CLONE_NEWCGROUP,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
		GidMappingsEnableSetgroups: false,
		Pdeathsig:                  syscall.SIGKILL, // the capsule dies with Luna, never orphaned
		// Only for the setup stage; it clears them, so the entry runs with none.
		AmbientCaps: []uintptr{unix.CAP_SYS_ADMIN, unix.CAP_SETPCAP},
	}
	// Pdeathsig fires when the forking thread exits; keep it pinned for the fork.
	runtime.LockOSThread()
	err = cmd.Start()
	runtime.UnlockOSThread()
	r.Close()
	if err != nil {
		return nil, fmt.Errorf("can't create the sandbox (are unprivileged user namespaces enabled?): %w", err)
	}
	p := &Proc{cmd: cmd, done: make(chan struct{})}
	if p.cg, err = intoCgroup(s, cmd.Process.Pid); err != nil {
		slog.Warn("no cgroup for capsule; using rlimits and the RSS watchdog", "capsule", s.Name, "err", err)
	} else {
		p.oomBase = p.parentOOMKills()
	}
	// The setup stage blocks reading this, so limits are in place before the entry runs.
	if err := json.NewEncoder(w).Encode(initConfig{Spec: s, Cgroup: p.cg != ""}); err != nil {
		p.Kill()
	}
	go p.wait()
	if p.cg == "" {
		go p.watchdog(s.Memory)
	}
	return p, nil
}

func (p *Proc) Pid() int { return p.cmd.Process.Pid }

// Done is closed once the process has exited and Exit is valid.
func (p *Proc) Done() <-chan struct{} { return p.done }
func (p *Proc) Exit() Exit            { return p.exit }

// Kill ends the capsule's whole process tree.
func (p *Proc) Kill() {
	p.killMu.Lock()
	defer p.killMu.Unlock()
	if p.cg != "" {
		os.WriteFile(filepath.Join(p.cg, "cgroup.kill"), []byte("1"), 0)
	}
	p.cmd.Process.Kill()
}

func (p *Proc) wait() {
	p.cmd.Wait()
	ws, _ := p.cmd.ProcessState.Sys().(syscall.WaitStatus)
	switch {
	case ws.Exited() && ws.ExitStatus() == 0:
	case ws.Exited():
		p.exit = Exit{Cause: "exit", Code: ws.ExitStatus()}
	case ws.Signaled():
		p.exit = Exit{Cause: "crash", Signal: ws.Signal().String()}
		if ws.Signal() == syscall.SIGSYS {
			p.exit.Cause = "policy"
		} else if ws.Signal() == syscall.SIGKILL && (p.oom.Load() || p.cg != "" && p.parentOOMKills() > p.oomBase) {
			p.exit.Cause = "oom"
		}
	}
	close(p.done)
}

// parentOOMKills reads oom_kill from the parent slice. The capsule's own cgroup is removed by
// systemd the moment it dies, but cgroup v2 counts events hierarchically, so the parent keeps them.
// shortcut: another app OOMing in the same slice at that instant would be misread as ours; rare.
func (p *Proc) parentOOMKills() int64 {
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(p.cg), "memory.events"))
	for l := range strings.Lines(string(b)) {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "oom_kill" {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			return n
		}
	}
	return 0
}

// watchdog enforces the memory cap where there's no cgroup.
// shortcut: polls PID 1's RSS only, once a second; children aren't counted. Fine as a fallback.
func (p *Proc) watchdog(limit int64) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-t.C:
			b, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", p.Pid()))
			if f := strings.Fields(string(b)); err == nil && len(f) > 1 {
				if pages, _ := strconv.ParseInt(f[1], 10, 64); pages*int64(os.Getpagesize()) > limit {
					p.oom.Store(true)
					p.Kill()
				}
			}
		}
	}
}

var (
	busOnce sync.Once
	bus     *dbus.Conn
	busErr  error
)

type unitProp struct {
	Name  string
	Value dbus.Variant
}

// intoCgroup moves pid into a new systemd user scope carrying the capsule's limits.
// systemd owns the user's delegated cgroup tree, so asking it is the clean way to get a child.
func intoCgroup(s Spec, pid int) (string, error) {
	busOnce.Do(func() { bus, busErr = dbus.ConnectSessionBus() })
	if busErr != nil {
		return "", busErr
	}
	unit := fmt.Sprintf("luna-capsule-%s-%d.scope", s.Name, pid)
	props := []unitProp{
		{"Description", dbus.MakeVariant("Luna capsule " + s.Name)},
		{"PIDs", dbus.MakeVariant([]uint32{uint32(pid)})},
		{"MemoryMax", dbus.MakeVariant(uint64(s.Memory))},
		{"MemorySwapMax", dbus.MakeVariant(uint64(0))}, // over the cap means killed, not swapping the host
		{"TasksMax", dbus.MakeVariant(uint64(s.Pids))},
		{"CPUQuotaPerSecUSec", dbus.MakeVariant(uint64(s.CPU * 1e6))},
		{"CollectMode", dbus.MakeVariant("inactive-or-failed")},
	}
	var job dbus.ObjectPath
	err := bus.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1").
		Call("org.freedesktop.systemd1.Manager.StartTransientUnit", 0, unit, "fail", props, []struct {
			Name  string
			Props []unitProp
		}{}).Store(&job)
	if err != nil {
		return "", err
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		f, err := os.Open(fmt.Sprintf("/proc/%d/cgroup", pid))
		if err != nil {
			return "", err
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if rest, ok := strings.CutPrefix(sc.Text(), "0::"); ok && strings.HasSuffix(rest, "/"+unit) {
				f.Close()
				return filepath.Join("/sys/fs/cgroup", rest), nil
			}
		}
		f.Close()
	}
	return "", errors.New("systemd didn't move the capsule into its scope in time")
}

// Doctor reports which isolation guarantees this machine can give.
func Doctor() []Check {
	var out []Check
	add := func(name string, ok bool, detail string) { out = append(out, Check{name, detail, ok}) }

	userns := readInt("/proc/sys/user/max_user_namespaces") > 0
	if readInt("/proc/sys/kernel/apparmor_restrict_unprivileged_userns") == 1 {
		add("user namespaces", false, "AppArmor restricts unprivileged user namespaces; Luna needs an AppArmor profile")
	} else if readInt("/proc/sys/kernel/unprivileged_userns_clone") == 0 && fileExists("/proc/sys/kernel/unprivileged_userns_clone") {
		add("user namespaces", false, "kernel.unprivileged_userns_clone=0; capsules can't start")
	} else {
		add("user namespaces", userns, "filesystem, process, network and IPC isolation")
	}

	abi, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if e != 0 {
		add("landlock", false, "unavailable; the mount namespace is the only filesystem layer")
	} else {
		add("landlock", true, fmt.Sprintf("ABI %d, second filesystem layer", abi))
	}
	add("seccomp", fileExists("/proc/sys/kernel/seccomp/actions_avail"), "syscall deny list")

	busOnce.Do(func() { bus, busErr = dbus.ConnectSessionBus() })
	ctrl, _ := os.ReadFile(userCgroup() + "/cgroup.controllers")
	have := strings.Fields(string(ctrl))
	missing := []string{}
	for _, c := range []string{"memory", "pids", "cpu"} {
		found := false
		for _, h := range have {
			found = found || h == c
		}
		if !found {
			missing = append(missing, c)
		}
	}
	switch {
	case busErr != nil:
		add("cgroup limits", false, "no systemd user session; memory cap is a watchdog, pids a rlimit, CPU uncapped")
	case len(missing) > 0:
		add("cgroup limits", false, "controllers not delegated to your user: "+strings.Join(missing, ", "))
	default:
		add("cgroup limits", true, "hard memory, CPU and process caps via systemd scopes")
	}
	add("network allowlists", true, "only the hosts a capsule lists, through Luna's egress proxy; no private addresses")
	add("data quota", false, "per-file size cap only, not a total quota")
	return out
}

func userCgroup() string {
	b, _ := os.ReadFile("/proc/self/cgroup")
	rest, _ := strings.CutPrefix(strings.TrimSpace(string(b)), "0::")
	if i := strings.Index(rest, ".service/"); i >= 0 && strings.Contains(rest, "/user@") {
		rest = rest[:i+len(".service")]
	}
	return filepath.Join("/sys/fs/cgroup", rest)
}

func readInt(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
