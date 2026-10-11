package sandbox

import (
	"path/filepath"
	"testing"
)

// Backends without a mount namespace (Windows) rewrite capsule paths to the mounted folders.
func TestHostPath(t *testing.T) {
	pkg, sock, exe := filepath.Join("C", "pkg"), filepath.Join("C", "run", "sock"), filepath.Join("C", "luna.exe")
	m := []Mount{{Src: pkg, Dst: "/pkg"}, {Src: sock, Dst: "/run/capsule"}, {Src: exe, Dst: "/luna"}}
	for in, want := range map[string]string{"/pkg/main.ts": filepath.Join(pkg, "main.ts"), "/pkg": pkg,
		"/run/capsule/links": filepath.Join(sock, "links"), "/luna": exe, "/lunatic": "/lunatic", "__bundle": "__bundle",
		"/pkgs/x": "/pkgs/x"} {
		if got, _ := hostPath(m, in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}
