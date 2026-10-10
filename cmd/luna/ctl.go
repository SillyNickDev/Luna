package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// Control socket: one JSON request and one JSON response per connection.
// ponytail: no peer-credential check yet (Security tab); add with the Phase 2 Linux backend.
type request struct {
	Cmd  string   `json:"cmd"`
	Args []string `json:"args"`
}

type response struct {
	Out string `json:"out,omitempty"`
	Err string `json:"err,omitempty"`
}

func listen(sock string) (net.Listener, error) {
	if len(sock) > 100 {
		return nil, fmt.Errorf("socket path is too long for a Unix socket (%d chars, max 100): %s", len(sock), sock)
	}
	if c, err := net.Dial("unix", sock); err == nil {
		c.Close()
		return nil, errors.New("luna is already running")
	}
	os.Remove(sock) // stale socket from a crash
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	os.Chmod(sock, 0o600)
	return ln, nil
}

func serve(ln net.Listener, handle func(request) (string, error)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			c.SetDeadline(time.Now().Add(10 * time.Second))
			var req request
			var resp response
			if err := json.NewDecoder(io.LimitReader(c, 64<<10)).Decode(&req); err != nil {
				resp.Err = "bad request"
			} else if out, err := handle(req); err != nil {
				resp.Err = err.Error()
			} else {
				resp.Out = out
			}
			json.NewEncoder(c).Encode(resp)
		}()
	}
}

func call(p paths, req request) error {
	c, err := net.Dial("unix", p.sock)
	if err != nil {
		return errors.New("the daemon isn't running; start it with: luna run")
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return err
	}
	var resp response
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return err
	}
	if resp.Err != "" {
		return errors.New(resp.Err)
	}
	fmt.Println(resp.Out)
	return nil
}
