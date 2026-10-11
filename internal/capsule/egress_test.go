package capsule

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEgressAllows(t *testing.T) {
	e := newEgress([]string{"api.spotify.com:443", "*.googleapis.com:443", "pronouns.alejo.io:443"}, io.Discard)
	for hp, ok := range map[string]bool{"api.spotify.com:443": true, "API.Spotify.com.:443": true, "api.spotify.com:80": false,
		"evil.com:443": false, "spotify.com:443": false, "translate.googleapis.com:443": true, "googleapis.com:443": false,
		"api.spotify.com.evil.com:443": false, "xgoogleapis.com:443": false} {
		h, p, _ := strings.Cut(hp, ":")
		if e.allows(h, p) != ok {
			t.Errorf("%s: want %v", hp, ok)
		}
	}
	for ip, ok := range map[string]bool{"1.1.1.1": true, "127.0.0.1": false, "10.1.2.3": false, "192.168.1.5": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "::1": false, "fd00::1": false,
		"fe80::1": false, "2606:4700::1111": true, "::ffff:127.0.0.1": false} {
		if publicIP(net.ParseIP(ip)) != ok {
			t.Errorf("%s: want public=%v", ip, ok)
		}
	}
}

func TestEgressProxy(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	_, port, _ := net.SplitHostPort(echo.Addr().String())

	dir, _ := os.MkdirTemp("", "eg")
	defer os.RemoveAll(dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "egress.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var log strings.Builder
	e := newEgress([]string{"api.example.com:" + port, "lan.example.com:" + port}, &log)
	e.lookup = func(_ context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}}, nil // both names point at the echo server
	}
	real := e.public
	e.public = func(ip net.IP) bool { return ip.IsLoopback() || real(ip) } // pretend loopback is the internet...
	go e.serve(ln)

	connect := func(host string) (string, net.Conn, *bufio.Reader) {
		c, err := net.Dial("unix", filepath.Join(dir, "egress.sock"))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(c, "CONNECT %s:%s HTTP/1.1\r\nHost: %s:%s\r\n\r\nping\n", host, port, host, port)
		br := bufio.NewReader(c) // one reader: the echo can arrive in the same packet as the 200
		line, _ := br.ReadString('\n')
		return strings.TrimSpace(line), c, br
	}
	status, c, br := connect("api.example.com")
	if status != "HTTP/1.1 200 Connection Established" {
		t.Fatalf("allowed host: %q", status)
	}
	var got string
	for !strings.HasSuffix(got, "ping\n") && err == nil { // skip the rest of the 200 response's header
		var line string
		line, err = br.ReadString('\n')
		got = line
	}
	c.Close()
	if !strings.HasSuffix(got, "ping\n") {
		t.Fatalf("tunnel: %q", got)
	}
	if status, c, _ := connect("evil.example.com"); !strings.Contains(status, "403") {
		t.Fatalf("unlisted host: %q", status)
	} else {
		c.Close()
	}
	e.public = real // ...and now it's private again, as it really is
	if status, c, _ := connect("lan.example.com"); !strings.Contains(status, "403") {
		t.Fatalf("listed name resolving to a private address: %q", status)
	} else {
		c.Close()
	}
}
