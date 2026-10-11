package capsule

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// egress is a capsule's only way out to the network: an HTTP CONNECT proxy on its own Unix
// socket that allows just the host:port entries in [capsule].net. The proxy resolves DNS
// itself, refuses private, loopback and link-local addresses (so a public name can't be
// pointed at the LAN or a cloud metadata service), and connects to the address it checked.
// TLS stays end to end: the proxy sees only where a connection goes and how many bytes.
type egress struct {
	allow  []string
	log    io.Writer
	lookup func(ctx context.Context, host string) ([]net.IPAddr, error)
	public func(net.IP) bool
	slots  chan struct{} // concurrent connections
}

const egressConns = 16

func newEgress(allow []string, log io.Writer) *egress {
	return &egress{allow: allow, log: log, lookup: net.DefaultResolver.LookupIPAddr, public: publicIP,
		slots: make(chan struct{}, egressConns)}
}

// allows reports whether host:port is on the list. "*.example.com:443" covers subdomains,
// not example.com itself.
func (e *egress) allows(host, port string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, a := range e.allow {
		h, p, _ := strings.Cut(strings.ToLower(a), ":")
		if p != port {
			continue
		}
		if h == host || strings.HasPrefix(h, "*.") && strings.HasSuffix(host, h[1:]) {
			return true
		}
	}
	return false
}

var cgnat = net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func publicIP(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !cgnat.Contains(ip) && !ip.Equal(net.IPv4(255, 255, 255, 255))
}

func (e *egress) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		select {
		case e.slots <- struct{}{}:
			go func() {
				defer func() { <-e.slots }()
				e.handle(c)
			}()
		default:
			fmt.Fprintf(c, "HTTP/1.1 429 Too Many Requests\r\n\r\nat most %d connections at once\n", egressConns)
			c.Close()
		}
	}
}

func refuse(c net.Conn, status int, why string) {
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\n\r\n%s\n", status, http.StatusText(status), why)
}

func (e *egress) handle(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	br := bufio.NewReader(io.LimitReader(c, 8<<10))
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		refuse(c, http.StatusMethodNotAllowed, "the egress proxy only does CONNECT")
		return
	}
	host, port, err := net.SplitHostPort(req.Host)
	if err != nil {
		refuse(c, http.StatusBadRequest, "CONNECT needs host:port")
		return
	}
	if !e.allows(host, port) {
		fmt.Fprintf(e.log, "luna: egress refused %s (not in [capsule].net)\n", req.Host)
		refuse(c, http.StatusForbidden, req.Host+" isn't in [capsule].net")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var target net.IP
	if ip := net.ParseIP(host); ip != nil {
		target = ip
	} else if addrs, err := e.lookup(ctx, host); err == nil {
		for _, a := range addrs {
			if e.public(a.IP) {
				target = a.IP
				break
			}
		}
		if target == nil && len(addrs) > 0 {
			target = addrs[0].IP // fails the check below, with the reason
		}
	}
	if target == nil {
		refuse(c, http.StatusBadGateway, "can't resolve "+host)
		return
	}
	if !e.public(target) {
		fmt.Fprintf(e.log, "luna: egress refused %s: %s isn't a public address\n", req.Host, target)
		refuse(c, http.StatusForbidden, host+" resolves to a private or local address")
		return
	}
	var d net.Dialer
	up, err := d.DialContext(ctx, "tcp", net.JoinHostPort(target.String(), port))
	if err != nil {
		refuse(c, http.StatusBadGateway, err.Error())
		return
	}
	defer up.Close()
	c.SetDeadline(time.Time{})
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// shortcut: no idle timeout or byte cap per connection; add them if a plugin holds connections open for no reason.
	var wg sync.WaitGroup
	var sent, received int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		sent, _ = io.Copy(up, io.MultiReader(br, c)) // br may hold bytes sent right after CONNECT
		if tc, ok := up.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()
	received, _ = io.Copy(c, up)
	c.Close()
	wg.Wait()
	fmt.Fprintf(e.log, "luna: egress %s (%s): sent %d B, received %d B\n", req.Host, target, sent, received)
}
