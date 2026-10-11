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
type request struct {
	Cmd  string   `json:"cmd"`
	Args []string `json:"args"`
}

type response struct {
	Out  string `json:"out,omitempty"`
	Err  string `json:"err,omitempty"`
	Code int    `json:"code,omitempty"` // exit code for the CLI, when not 1
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
			if !peerOK(c) {
				return
			}
			c.SetDeadline(time.Now().Add(40 * time.Second)) // plugin commands may take 30 s
			var req request
			var resp response
			if err := json.NewDecoder(io.LimitReader(c, 64<<10)).Decode(&req); err != nil {
				resp.Err = "bad request"
			} else if out, err := handle(req); err != nil {
				resp.Err = err.Error()
				var ee exitErr
				if errors.As(err, &ee) {
					resp.Code = ee.code
				}
			} else {
				resp.Out = out
			}
			json.NewEncoder(c).Encode(resp)
		}()
	}
}

func call(p paths, req request) error {
	out, err := callOut(p, req)
	if err == nil {
		fmt.Println(out)
	}
	return err
}

func callOut(p paths, req request) (string, error) {
	c, err := net.Dial("unix", p.sock)
	if err != nil {
		return "", exitErr{3, "the daemon isn't running; start it with: luna run"}
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return "", err
	}
	var resp response
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return "", err
	}
	if resp.Err != "" {
		if resp.Code != 0 {
			return "", exitErr{resp.Code, resp.Err}
		}
		return "", errors.New(resp.Err)
	}
	return resp.Out, nil
}
