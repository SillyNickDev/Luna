package capsule

import (
	"log/slog"
	"net"
	"slices"
	"time"
)

const (
	linkMaxBody = 64 << 10
	linkRate    = 50 // calls per second per link
	linkTimeout = 5 * time.Second
)

// acceptLink serves consumer connections on one of its link sockets. The socket belongs
// to the broker, so the consumer never gets a path into the provider.
func (c *Capsule) acceptLink(ln net.Listener, use string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go c.serveLink(conn, use)
	}
}

func (c *Capsule) serveLink(conn net.Conn, use string) {
	defer conn.Close()
	fc := NewConn(conn)
	provID, name, _ := SplitUse(use)
	tokens, last := float64(linkRate), time.Now()
	for {
		f, err := fc.Read()
		if err != nil {
			return
		}
		if f.T != "call" {
			continue
		}
		reply := func(r Result) { fc.Write("result", f.ID, r) }
		var call Call
		if err := Decode(f.B, &call); err != nil {
			reply(Result{Error: "bad_call"})
			continue
		}
		if len(call.Body) > linkMaxBody {
			reply(Result{Error: "too_large"})
			continue
		}
		now := time.Now()
		tokens = min(linkRate, tokens+now.Sub(last).Seconds()*linkRate)
		last = now
		if tokens < 1 {
			reply(Result{Error: "rate_limited"})
			continue
		}
		tokens--
		r := c.rt.forward(c.ID, provID, name, call)
		slog.Debug("link call", "caller", c.ID, "provider", provID, "link", name, "method", call.Method,
			"bytes", len(call.Body), "ok", r.OK, "error", r.Error, "ms", time.Since(now).Milliseconds())
		reply(r)
	}
}

// forward sends a call to the provider, stamped with the caller's ID, and waits for its result.
func (rt *Runtime) forward(caller, provID, name string, call Call) Result {
	p := rt.Get(provID)
	if p == nil || !slices.Contains(p.Pkg.Manifest.Links.Provides, name) {
		return Result{Error: "link_unavailable"}
	}
	p.mu.Lock()
	ctl := p.ctl
	if ctl == nil {
		p.mu.Unlock()
		return Result{Error: "link_unavailable"}
	}
	p.nextCall++
	id := p.nextCall
	ch := make(chan Result, 1)
	p.calls[id] = ch
	p.mu.Unlock()

	call.Caller = caller // whatever the consumer put here is overwritten
	err := ctl.Write("call", id, LinkCall{Link: name, Call: call})
	if err == nil {
		select {
		case r, ok := <-ch:
			if ok {
				if len(r.Body) > linkMaxBody {
					return Result{Error: "too_large"}
				}
				return r
			}
		case <-time.After(linkTimeout):
			p.mu.Lock()
			if p.calls != nil {
				delete(p.calls, id)
			}
			p.mu.Unlock()
			return Result{Error: "timeout"}
		}
	}
	return Result{Error: "link_unavailable"}
}

// LinkCall is a call as the provider sees it on its control socket.
type LinkCall struct {
	Link string `cbor:"link"`
	Call
}

// linkEvent tells every running consumer of p's links that they came up or went down.
func (rt *Runtime) linkEvent(p *Capsule, t string) {
	for _, c := range rt.List() {
		c.mu.Lock()
		ctl := c.ctl
		c.mu.Unlock()
		if ctl == nil || c == p {
			continue
		}
		for _, use := range c.Uses {
			if id, name, _ := SplitUse(use); id == p.ID && slices.Contains(p.Pkg.Manifest.Links.Provides, name) {
				ctl.Write(t, 0, LinkName{Name: use})
			}
		}
	}
}

// linksUp tells a capsule that has just started which of its links already have a running provider.
func (rt *Runtime) linksUp(c *Capsule, ctl *Conn) {
	for _, use := range c.Uses {
		id, name, _ := SplitUse(use)
		if p := rt.Get(id); p != nil && p.State() == Running && slices.Contains(p.Pkg.Manifest.Links.Provides, name) {
			ctl.Write("link_up", 0, LinkName{Name: use})
		}
	}
}
