package capsule

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testManifest = `spec = "0.1"
id = "dev.test.hello"
version = "0.1.0"
runtime = "native"
entry = "hello"
limits = { memory = "32m", pids = 8 }
[links]
provides = ["greet"]
uses = ["dev.test.other:show"]
`

func writePkg(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestManifest(t *testing.T) {
	m, err := LoadManifest(writePkg(t, map[string]string{"capsule.toml": testManifest}))
	if err != nil {
		t.Fatal(err)
	}
	if m.Limits.Memory != 32<<20 || m.Limits.Pids != 8 || m.Limits.Files != 256 || m.Net != nil {
		t.Fatalf("limits/defaults wrong: %+v", m.Limits)
	}
	merged := `id = "dev.test.hello"
version = "1.0.0"
name = "ignored by the runtime"
[capsule]
runtime = "python-3.12"
entry = "main.py"
net = ["api.example.com:443"]
[links]
uses = ["dev.sable.overlay:show"]
`
	m, err = LoadManifest(writePkg(t, map[string]string{"plugin.toml": merged}))
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "dev.test.hello" || len(m.Net) != 1 || m.Links.Uses[0] != "dev.sable.overlay:show" {
		t.Fatalf("merged manifest wrong: %+v", m)
	}
	for _, bad := range []string{
		strings.Replace(testManifest, `entry = "hello"`, `entry = "../../bin/sh"`, 1),
		strings.Replace(testManifest, `id = "dev.test.hello"`, `id = "Hello"`, 1),
		testManifest + "surprise = 1\n",
	} {
		if _, err := LoadManifest(writePkg(t, map[string]string{"capsule.toml": bad})); err == nil {
			t.Errorf("accepted bad manifest:\n%s", bad)
		}
	}
}

func TestPackVerifyTamper(t *testing.T) {
	dir := writePkg(t, map[string]string{"capsule.toml": testManifest, "hello": "#!bin", "assets/a.txt": "a"})
	out := filepath.Join(t.TempDir(), "hello.capsule")
	if err := Pack(dir, out); err != nil {
		t.Fatal(err)
	}
	store := t.TempDir()
	u, err := AddToStore(store, out)
	if err != nil {
		t.Fatal(err)
	}
	if u.Manifest.ID != "dev.test.hello" || filepath.Base(u.Dir) != u.Hash {
		t.Fatalf("bad unpack: %+v", u)
	}
	if again, err := AddToStore(store, dir); err != nil || again.Hash != u.Hash {
		t.Fatalf("folder install should give the same hash: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "assets/a.txt"), []byte("evil"), 0o644)
	if _, err := AddToStore(store, dir); err == nil || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("tampered package accepted: %v", err)
	}
	os.Symlink("/etc/passwd", filepath.Join(dir, "link"))
	if _, err := AddToStore(store, dir); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestUnzipRejectsEscapes(t *testing.T) {
	for _, name := range []string{"../evil", "/abs", "a/../../evil", `a\..\evil`} {
		p := filepath.Join(t.TempDir(), "x.capsule")
		f, _ := os.Create(p)
		zw := zip.NewWriter(f)
		w, _ := zw.Create(name)
		w.Write([]byte("x"))
		zw.Close()
		f.Close()
		if err := unzip(p, t.TempDir()); err == nil {
			t.Errorf("unzip accepted %q", name)
		}
	}
}

func FuzzManifest(f *testing.F) {
	f.Add(testManifest)
	f.Fuzz(func(t *testing.T, s string) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "capsule.toml"), []byte(s), 0o644)
		if m, err := LoadManifest(dir); err == nil && !localPath(m.Entry) {
			t.Fatalf("accepted escaping entry %q", m.Entry)
		}
	})
}
