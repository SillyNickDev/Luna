//go:build linux

package sandbox

import "golang.org/x/sys/unix"

const auditArch = unix.AUDIT_ARCH_AARCH64

var archGuard []unix.SockFilter
