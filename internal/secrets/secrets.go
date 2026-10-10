// Package secrets stores tokens in the OS keyring (Secret Service, Credential Manager, Keychain).
// Without a keyring they're AES-GCM encrypted in files, under a key file only the user can read.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
)

var ErrNotFound = errors.New("secret not found")

type Store struct {
	dir     string
	service string // keyring service, per data dir so two Luna instances don't collide
	keyring bool
}

func Open(dataDir string) *Store {
	s := &Store{dir: filepath.Join(dataDir, "secrets"), service: "luna " + dataDir}
	_, err := keyring.Get(s.service, "probe")
	s.keyring = errors.Is(err, keyring.ErrNotFound)
	return s
}

// UsesKeyring is false when secrets fall back to encrypted files, which is weaker: warn about it.
func (s *Store) UsesKeyring() bool { return s.keyring }

// Get checks the keyring, then the file fallback (the keyring may be missing,
// e.g. when the daemon runs without a desktop session).
func (s *Store) Get(name string) ([]byte, error) {
	if s.keyring {
		v, err := keyring.Get(s.service, name)
		if err == nil {
			return []byte(v), nil
		}
		if !errors.Is(err, keyring.ErrNotFound) {
			return nil, err
		}
	}
	ct, err := os.ReadFile(s.file(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	aead, err := s.aead()
	if err != nil {
		return nil, err
	}
	if len(ct) < aead.NonceSize() {
		return nil, fmt.Errorf("secret %s is corrupt", name)
	}
	return aead.Open(nil, ct[:aead.NonceSize()], ct[aead.NonceSize():], []byte(name))
}

func (s *Store) Set(name string, v []byte) error {
	if s.keyring {
		os.Remove(s.file(name)) // don't leave an older copy behind
		return keyring.Set(s.service, name, string(v))
	}
	aead, err := s.aead()
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	rand.Read(nonce)
	return writeAtomic(s.file(name), aead.Seal(nonce, nonce, v, []byte(name)))
}

func (s *Store) Delete(name string) error {
	if s.keyring {
		if err := keyring.Delete(s.service, name); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return err
		}
	}
	if err := os.Remove(s.file(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) file(name string) string { return filepath.Join(s.dir, name+".enc") }

// aead loads the key file, creating it on first use.
func (s *Store) aead() (cipher.AEAD, error) {
	path := filepath.Join(s.dir, "secret.key")
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		rand.Read(key)
		err = writeAtomic(path, key)
	}
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s is corrupt", path)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// writeAtomic writes an owner-only file via temp file and rename.
func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
