//go:build !linux

package main

import (
	"bufio"
	"os"
	"strings"
)

// readSecret reads a line from stdin. shortcut: it echoes off Linux; hide it there when Windows users ask.
func readSecret() (string, error) {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line), err
}
