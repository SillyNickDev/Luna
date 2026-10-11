//go:build linux

package sandbox

import "golang.org/x/sys/unix"

const auditArch = unix.AUDIT_ARCH_X86_64

// archGuard refuses x32 syscalls (nr with bit 30 set), which would bypass the deny list.
var archGuard = []unix.SockFilter{
	{Code: unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K, Jf: 1, K: 0x40000000},
	{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
}
