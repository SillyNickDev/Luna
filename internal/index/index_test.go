package index

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignAndParse(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	pubText, err := Keygen(key)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(key); fi.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode %v", fi.Mode().Perm())
	}
	ix := &Index{Plugins: []Entry{{ID: "dev.fox.songs", Version: "1.0.0", File: "songs-1.0.0.capsule", Hash: "aa"}}}
	if err := Sign(ix, dir, key); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "index.json"))
	sig, _ := os.ReadFile(filepath.Join(dir, "index.json.sig"))
	pub, err := ParsePublicKey(pubText)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(raw, sig, pub)
	if err != nil || len(got.Plugins) != 1 || got.Plugins[0].Hash != "aa" {
		t.Fatalf("%+v %v", got, err)
	}
	tampered := bytes.Replace(raw, []byte(`"aa"`), []byte(`"bb"`), 1)
	if _, err := Parse(tampered, sig, pub); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("a tampered index was accepted: %v", err)
	}
	other, _ := Keygen(filepath.Join(dir, "other"))
	otherPub, _ := ParsePublicKey(other)
	if _, err := Parse(raw, sig, otherPub); err == nil {
		t.Fatal("an index signed by another key was accepted")
	}
}

func TestFind(t *testing.T) {
	ix := &Index{
		Plugins: []Entry{{ID: "dev.fox.songs", Version: "0.9.3"}, {ID: "dev.fox.songs", Version: "0.10.0"},
			{ID: "dev.fox.songs", Version: "0.11.0"}, {ID: "dev.owl.songs", Version: "1.0.0"}, {ID: "dev.fox.hats", Version: "1.0.0"}},
		Revoked: []Revoked{{ID: "dev.fox.songs", Version: "0.11.0", Reason: "leaks tokens"}, {ID: "dev.fox.hats", Version: "*", Reason: "malware"}},
	}
	if e, err := ix.Find("dev.fox.songs"); err != nil || e.Version != "0.10.0" {
		t.Fatalf("newest unrevoked: %+v %v", e, err)
	}
	if _, err := ix.Find("songs"); err == nil || !strings.Contains(err.Error(), "dev.fox.songs, dev.owl.songs") {
		t.Fatalf("ambiguous short name: %v", err)
	}
	if _, err := ix.Find("hats"); err == nil {
		t.Fatal("found a plugin revoked at every version")
	}
	if ix.IsRevoked("dev.fox.songs", "0.11.0") != "leaks tokens" || ix.IsRevoked("dev.fox.songs", "0.10.0") != "" {
		t.Fatal("revocation by version")
	}
}

func TestJoin(t *testing.T) {
	for _, c := range []struct{ base, file, want string }{
		{"https://example.com/luna-index", "a.capsule", "https://example.com/luna-index/a.capsule"},
		{"https://example.com/luna-index/", "a.capsule", "https://example.com/luna-index/a.capsule"},
		{"http://127.0.0.1:8080", "a.capsule", "http://127.0.0.1:8080/a.capsule"},
	} {
		if got, err := join(c.base, c.file); err != nil || got != c.want {
			t.Errorf("%s + %s: %q %v", c.base, c.file, got, err)
		}
	}
	for _, c := range [][2]string{{"http://example.com", "a"}, {"https://example.com", "../a"}, {"https://example.com", "https://evil.com/a"},
		{"https://example.com", "/etc/a"}} {
		if _, err := join(c[0], c[1]); err == nil {
			t.Errorf("%s + %s accepted", c[0], c[1])
		}
	}
	if !Newer("0.10.0", "0.9.3") || Newer("1.0", "1.0.0") || !Newer("2", "1.9.9") {
		t.Fatal("Newer")
	}
}
