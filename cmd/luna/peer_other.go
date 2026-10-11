//go:build !linux

package main

import "net"

// shortcut: no peer check off Linux yet (getpeereid / pipe tokens come with those backends);
// the socket file is owner-only.
func peerOK(net.Conn) bool  { return true }
func ownedDir(string) error { return nil }
