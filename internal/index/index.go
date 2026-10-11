// Package index is the plugin index: a signed list of packages at a base URL (index.json,
// index.json.sig, and the .capsule files). The signature is Ed25519 over index.json's
// bytes, by a key the maintainer keeps offline; Luna trusts only the public key in
// [index].public_key. Each entry names its package's content hash (the one capsule.lock
// produces), so a downloaded package must match exactly, and revoked versions are listed
// in the same signed file.
package index

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type Entry struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version"`
	Runtime string `json:"runtime"`
	File    string `json:"file"` // relative to the index's URL
	Hash    string `json:"hash"` // package content hash, as luna capsule verify prints it
	Size    int64  `json:"size"`
}

type Revoked struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Reason  string `json:"reason"`
}

type Index struct {
	Format    int       `json:"format"`
	Generated time.Time `json:"generated"`
	Plugins   []Entry   `json:"plugins"`
	Revoked   []Revoked `json:"revoked"`
}

// IsRevoked reports why a package version was revoked, or "".
func (ix *Index) IsRevoked(id, version string) string {
	for _, r := range ix.Revoked {
		if r.ID == id && (r.Version == version || r.Version == "*") {
			return r.Reason
		}
	}
	return ""
}

// Find is the newest unrevoked entry for an ID, or a short name ("songs" finds "dev.x.songs").
func (ix *Index) Find(name string) (*Entry, error) {
	var hits []Entry
	for _, e := range ix.Plugins {
		if (e.ID == name || strings.HasSuffix(e.ID, "."+name)) && ix.IsRevoked(e.ID, e.Version) == "" {
			hits = append(hits, e)
		}
	}
	ids := map[string]bool{}
	for _, e := range hits {
		ids[e.ID] = true
	}
	switch {
	case len(hits) == 0:
		return nil, fmt.Errorf("%s isn't in the plugin index (luna capsule search shows what is)", name)
	case len(ids) > 1:
		var all []string
		for id := range ids {
			all = append(all, id)
		}
		slices.Sort(all)
		return nil, fmt.Errorf("%q matches several plugins; use the full ID: %s", name, strings.Join(all, ", "))
	}
	best := hits[0]
	for _, e := range hits[1:] {
		if Newer(e.Version, best.Version) {
			best = e
		}
	}
	return &best, nil
}

// Newer compares dotted versions numerically ("0.10.0" is newer than "0.9.3").
func Newer(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			fmt.Sscan(pa[i], &x)
		}
		if i < len(pb) {
			fmt.Sscan(pb[i], &y)
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// ParsePublicKey reads a base64 Ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("[index] public_key must be a base64 Ed25519 public key (luna index keygen prints one)")
	}
	return b, nil
}

// Parse checks the signature over raw, then decodes it.
func Parse(raw, sig []byte, pub ed25519.PublicKey) (*Index, error) {
	s, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || !ed25519.Verify(pub, raw, s) {
		return nil, errors.New("the plugin index's signature doesn't match its public key; refusing it")
	}
	var ix Index
	if err := json.Unmarshal(raw, &ix); err != nil {
		return nil, fmt.Errorf("plugin index: %w", err)
	}
	if ix.Format != 1 {
		return nil, fmt.Errorf("plugin index format %d isn't supported; update luna", ix.Format)
	}
	return &ix, nil
}

var hc = &http.Client{Timeout: 2 * time.Minute}

func get(ctx context.Context, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", u, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is over %d MiB", u, limit>>20)
	}
	return b, err
}

func join(base, file string) (string, error) {
	b, err := url.Parse(strings.TrimSuffix(base, "/") + "/")
	if err != nil {
		return "", err
	}
	if b.Scheme != "https" && !(b.Scheme == "http" && (b.Hostname() == "127.0.0.1" || b.Hostname() == "localhost")) {
		return "", errors.New("[index] url must be https")
	}
	f, err := url.Parse(file)
	if err != nil || f.IsAbs() || strings.Contains(file, "..") || strings.HasPrefix(file, "/") {
		return "", fmt.Errorf("index entry file %q must be a plain relative path", file)
	}
	return b.ResolveReference(f).String(), nil
}

// Fetch downloads and verifies the index at base.
func Fetch(ctx context.Context, base, publicKey string) (*Index, error) {
	pub, err := ParsePublicKey(publicKey)
	if err != nil {
		return nil, err
	}
	iu, err := join(base, "index.json")
	if err != nil {
		return nil, err
	}
	raw, err := get(ctx, iu, 8<<20)
	if err != nil {
		return nil, err
	}
	sig, err := get(ctx, iu+".sig", 4<<10)
	if err != nil {
		return nil, err
	}
	return Parse(raw, sig, pub)
}

// Download fetches an entry's .capsule file into dir. The caller installs it, which
// computes its content hash; it must equal e.Hash.
func Download(ctx context.Context, base string, e *Entry, dir string) (string, error) {
	u, err := join(base, e.File)
	if err != nil {
		return "", err
	}
	b, err := get(ctx, u, 64<<20)
	if err != nil {
		return "", err
	}
	if e.Size > 0 && int64(len(b)) != e.Size {
		return "", fmt.Errorf("%s: %d bytes, the index says %d", e.File, len(b), e.Size)
	}
	dst := filepath.Join(dir, filepath.Base(e.File))
	return dst, os.WriteFile(dst, b, 0o600)
}

// ---------- maintainer side ----------

// Keygen writes a new private key to path (mode 0600) and returns the public key to publish.
func Keygen(path string) (string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0o600); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}

// Sign writes ix as dir/index.json and its signature as dir/index.json.sig.
func Sign(ix *Index, dir, keyPath string) error {
	k, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}
	priv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(k)))
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		return errors.New(keyPath + " isn't a key from luna index keygen")
	}
	ix.Format, ix.Generated = 1, time.Now().UTC()
	if ix.Revoked == nil {
		ix.Revoked = []Revoked{}
	}
	raw, err := json.MarshalIndent(ix, "", "  ")
	if err != nil {
		return err
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.PrivateKey(priv), raw))
	if err := os.WriteFile(filepath.Join(dir, "index.json"), raw, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "index.json.sig"), []byte(sig+"\n"), 0o644)
}
