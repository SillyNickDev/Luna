package capsule

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
)

// Egress requests come from capsules: whatever they send, nothing off the allowlist is
// ever looked up or dialled.
func FuzzEgress(f *testing.F) {
	f.Add("CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n")
	f.Add("CONNECT evil.com:443 HTTP/1.1\r\n\r\n")
	f.Add("GET http://api.example.com/ HTTP/1.1\r\n\r\n")
	f.Add("CONNECT [::1]:443 HTTP/1.1\r\n\r\n")
	f.Fuzz(func(t *testing.T, req string) {
		e := newEgress([]string{"api.example.com:443"}, io.Discard)
		e.lookup = func(_ context.Context, host string) ([]net.IPAddr, error) {
			if !strings.EqualFold(strings.TrimSuffix(host, "."), "api.example.com") {
				t.Fatalf("looked up %q, which isn't allowed", host)
			}
			return nil, io.EOF // never actually connect
		}
		a, b := net.Pipe()
		go func() { a.Write([]byte(req)); a.Close() }() // closing: a half-sent request ends now, not at the 15 s deadline
		e.handle(b)
	})
}
