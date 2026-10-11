package main

import (
	"errors"
	"syscall"
)

func osProbe(cmd, arg string, n int) error {
	switch cmd {
	case "unshare":
		return syscall.Unshare(syscall.CLONE_NEWUSER)
	case "mount":
		return syscall.Mount("tmpfs", "/data", "tmpfs", 0, "")
	case "kill":
		return syscall.Kill(n, syscall.SIGKILL)
	}
	return errors.New("unknown probe " + cmd)
}
