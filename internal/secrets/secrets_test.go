package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileFallback(t *testing.T) {
	dir := t.TempDir()
	s := &Store{dir: filepath.Join(dir, "secrets")} // force the no-keyring path

	if _, err := s.Get("bot"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing secret: err = %v", err)
	}
	if err := s.Set("bot", []byte("tok")); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(s.file("bot"))
	if string(raw) == "tok" || len(raw) < 12 {
		t.Fatal("secret stored in plain text")
	}
	if v, err := s.Get("bot"); err != nil || string(v) != "tok" {
		t.Fatalf("Get = %q, %v", v, err)
	}
	os.Rename(s.file("bot"), s.file("broadcaster")) // ciphertext is bound to its name
	if _, err := s.Get("broadcaster"); err == nil {
		t.Fatal("swapped secret decrypted")
	}
	if err := s.Delete("broadcaster"); err != nil {
		t.Fatal(err)
	}
}
