package main

import (
	"fmt"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// peerOK refuses a control-socket client running as another user, even if file permissions are wrong.
func peerOK(c net.Conn) bool {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	var cred *unix.Ucred
	raw.Control(func(fd uintptr) { cred, err = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	return err == nil && int(cred.Uid) == os.Getuid()
}

// ownedDir refuses a socket directory someone else owns or others can write to.
func ownedDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	st := fi.Sys().(*syscall.Stat_t)
	if int(st.Uid) != os.Getuid() || fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s must be owned by you and not writable by others", dir)
	}
	return nil
}
