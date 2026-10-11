//go:build linux

package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Init is the capsule's setup stage, run as `luna __capsule-init` inside the new namespaces.
// It builds the root, locks it down and execs the entry. It never returns.
func Init() {
	err := setup()
	fmt.Fprintln(os.Stderr, "capsule setup failed:", err)
	os.Exit(125)
}

var devices = []string{"/dev/null", "/dev/zero", "/dev/urandom"}

func setup() error {
	runtime.LockOSThread() // Landlock and no_new_privs are per thread; exec happens on this one
	f := os.NewFile(3, "config")
	var c initConfig
	if err := json.NewDecoder(f).Decode(&c); err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	f.Close()

	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("private mounts: %w", err)
	}
	root := c.Root
	if err := unix.Mount("tmpfs", root, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "size=1m,mode=0755"); err != nil {
		return fmt.Errorf("root tmpfs: %w", err)
	}
	for _, m := range c.Mounts {
		dst := filepath.Join(root, m.Dst)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if err := bind(m.Src, dst, m.RW, false); err != nil {
			return fmt.Errorf("mount %s: %w", m.Dst, err)
		}
	}
	os.MkdirAll(filepath.Join(root, "dev"), 0o755)
	for _, d := range devices {
		dst := filepath.Join(root, d)
		if err := os.WriteFile(dst, nil, 0o666); err != nil {
			return err
		}
		if err := bind(d, dst, d == "/dev/null", true); err != nil {
			return fmt.Errorf("mount %s: %w", d, err)
		}
	}
	os.MkdirAll(filepath.Join(root, "proc"), 0o555)
	procOK := unix.Mount("proc", filepath.Join(root, "proc"), "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "") == nil

	if err := os.Chdir(root); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detaching the host root: %w", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
		return fmt.Errorf("read-only root: %w", err)
	}
	if err := os.Chdir(c.Dir); err != nil {
		return err
	}
	unix.Sethostname([]byte("capsule"))

	rl := map[int]uint64{unix.RLIMIT_NOFILE: uint64(c.Files), unix.RLIMIT_CORE: 0}
	if c.FileSize > 0 {
		rl[unix.RLIMIT_FSIZE] = uint64(c.FileSize)
	}
	if !c.Cgroup {
		// shortcut: without a cgroup NPROC counts per user namespace, a loose stand-in for pids.max.
		rl[unix.RLIMIT_NPROC] = uint64(c.Pids)
	}
	for res, v := range rl {
		if err := unix.Setrlimit(res, &unix.Rlimit{Cur: v, Max: v}); err != nil {
			return fmt.Errorf("rlimit %d: %w", res, err)
		}
	}

	if err := landlock(c.Mounts, procOK); err != nil {
		return fmt.Errorf("landlock: %w", err)
	}
	for cp := 0; cp <= 63; cp++ {
		unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(cp), 0, 0, 0)
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("dropping capabilities: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	if err := seccomp(); err != nil {
		return fmt.Errorf("seccomp: %w", err)
	}
	return syscall.Exec(c.Argv[0], c.Argv, c.Env)
}

// bind mounts src on dst, then remounts it nosuid (and read-only unless rw). Flags the host
// mount already has are kept, since an unprivileged user namespace can't clear them.
func bind(src, dst string, rw, dev bool) error {
	if err := unix.Mount(src, dst, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return err
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dst, &st); err != nil {
		return err
	}
	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_NOSUID)
	for stf, msf := range map[int64]uintptr{
		unix.ST_RDONLY: unix.MS_RDONLY, unix.ST_NODEV: unix.MS_NODEV, unix.ST_NOEXEC: unix.MS_NOEXEC,
		unix.ST_NOATIME: unix.MS_NOATIME, unix.ST_NODIRATIME: unix.MS_NODIRATIME, unix.ST_RELATIME: unix.MS_RELATIME,
	} {
		if st.Flags&stf != 0 {
			flags |= msf
		}
	}
	if !rw {
		flags |= unix.MS_RDONLY
	} else {
		flags |= unix.MS_NOEXEC // writable means data, never code
	}
	if !dev {
		flags |= unix.MS_NODEV
	}
	return unix.Mount("", dst, "", flags, "")
}

const (
	llRead    = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
	llExec    = unix.LANDLOCK_ACCESS_FS_EXECUTE
	llFileRW  = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE
	llFileAll = llFileRW | unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
)

// landlock confines the capsule to its mounts, as a second layer under the mount
// namespace. On ABI 4+ it also denies all TCP, and on 6+ signals and abstract sockets outside it.
func landlock(mounts []Mount, proc bool) error {
	abi, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if e != 0 {
		return nil // not in this kernel; luna capsule doctor says so
	}
	attr := unix.LandlockRulesetAttr{Access_fs: 1<<13 - 1}
	if abi >= 2 {
		attr.Access_fs |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		attr.Access_fs |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 4 {
		attr.Access_net = unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP
	}
	if abi >= 5 {
		attr.Access_fs |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	if abi >= 6 {
		attr.Scoped = unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL
	}
	fd, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if e != 0 {
		return e
	}
	defer unix.Close(int(fd))
	allow := func(path string, access uint64) error {
		pfd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		defer unix.Close(pfd)
		rule := unix.LandlockPathBeneathAttr{Allowed_access: access & attr.Access_fs, Parent_fd: int32(pfd)}
		if _, _, e := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); e != 0 {
			return fmt.Errorf("%s: %w", path, e)
		}
		return nil
	}
	for _, m := range mounts {
		access := uint64(llRead | llExec)
		if m.RW {
			access = attr.Access_fs &^ llExec
		}
		if err := allow(m.Dst, access); err != nil {
			return err
		}
	}
	for _, d := range devices {
		access := uint64(unix.LANDLOCK_ACCESS_FS_READ_FILE)
		if d == "/dev/null" {
			access = llFileAll &^ llExec
		}
		if err := allow(d, access); err != nil {
			return err
		}
	}
	if proc {
		if err := allow("/proc", llRead); err != nil {
			return err
		}
	}
	if _, _, e := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); e != 0 {
		return e
	}
	return nil
}

// seccomp installs default-allow with a deny list (Capsules Spec, Linux): denied calls fail
// with EPERM; clone3 gets ENOSYS so libc falls back to clone, whose namespace flags we can check.
func seccomp() error {
	const (
		ldNR   = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeq    = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		jset   = unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K
		ret    = unix.BPF_RET | unix.BPF_K
		eperm  = unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)
		enosys = unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)
		nsMask = unix.CLONE_NEWUSER | unix.CLONE_NEWNS | unix.CLONE_NEWNET | unix.CLONE_NEWPID |
			unix.CLONE_NEWIPC | unix.CLONE_NEWUTS | unix.CLONE_NEWCGROUP | unix.CLONE_NEWTIME
	)
	p := []unix.SockFilter{
		{Code: ldNR, K: 4}, // seccomp_data.arch
		{Code: jeq, Jt: 1, K: auditArch},
		{Code: ret, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: ldNR, K: 0}, // seccomp_data.nr
	}
	p = append(p, archGuard...)
	for _, nr := range denied {
		p = append(p, unix.SockFilter{Code: jeq, Jf: 1, K: uint32(nr)}, unix.SockFilter{Code: ret, K: eperm})
	}
	p = append(p,
		unix.SockFilter{Code: jeq, Jf: 1, K: unix.SYS_CLONE3},
		unix.SockFilter{Code: ret, K: enosys},
		unix.SockFilter{Code: jeq, Jf: 3, K: unix.SYS_CLONE},
		unix.SockFilter{Code: ldNR, K: 16}, // low half of args[0], the clone flags
		unix.SockFilter{Code: jset, Jf: 1, K: nsMask},
		unix.SockFilter{Code: ret, K: eperm},
		unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_ALLOW},
	)
	prog := unix.SockFprog{Len: uint16(len(p)), Filter: &p[0]}
	if _, _, e := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&prog))); e != 0 {
		return e
	}
	return nil
}

var denied = []uintptr{
	unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV,
	unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT,
	unix.SYS_FSOPEN, unix.SYS_FSMOUNT, unix.SYS_FSPICK, unix.SYS_MOVE_MOUNT, unix.SYS_OPEN_TREE, unix.SYS_MOUNT_SETATTR,
	unix.SYS_UNSHARE, unix.SYS_SETNS,
	unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_USERFAULTFD,
	unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY,
	unix.SYS_KEXEC_LOAD, unix.SYS_KEXEC_FILE_LOAD, unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE,
	unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_SWAPON, unix.SYS_SWAPOFF, unix.SYS_REBOOT, unix.SYS_ACCT,
}
