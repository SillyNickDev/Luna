package main

import (
	"bufio"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// readSecret reads a line from the terminal without echoing it: API keys shouldn't show
// on a screen that may be streaming.
func readSecret() (string, error) {
	fd := int(os.Stdin.Fd())
	if old, err := unix.IoctlGetTermios(fd, unix.TCGETS); err == nil {
		quiet := *old
		quiet.Lflag &^= unix.ECHO
		unix.IoctlSetTermios(fd, unix.TCSETS, &quiet)
		defer unix.IoctlSetTermios(fd, unix.TCSETS, old)
		defer os.Stdout.WriteString("\n")
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line), err
}
