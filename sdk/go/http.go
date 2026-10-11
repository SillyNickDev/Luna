package luna

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// newHTTP is a client whose every connection goes through the capsule's egress proxy, which
// lets through only the host:port entries in [capsule].net. HTTPS only: the proxy tunnels
// TLS and never sees what's inside.
func newHTTP() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: "egress"}),
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				sock := os.Getenv("CAPSULE_EGRESS")
				if sock == "" {
					return nil, errors.New("no network: list the hosts this plugin needs in [capsule].net, like \"api.example.com:443\"")
				}
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 2,
		},
	}
}
