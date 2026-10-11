// Package sdkfs installs an SDK embedded in the luna binary onto disk, so capsules can
// mount it read-only at /sdk, always matching the Luna version that runs them.
package sdkfs

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Install writes fsys under base/<family>-<hash>/ (once per SDK version) and returns that folder.
func Install(fsys fs.FS, base, family string) (string, error) {
	h := sha256.New()
	var paths []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.Contains(p, "__pycache__") || strings.HasSuffix(p, ".go") {
			return err
		}
		b, _ := fs.ReadFile(fsys, p)
		h.Write([]byte(p + "\x00"))
		h.Write(b)
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, family+"-"+hex.EncodeToString(h.Sum(nil))[:16])
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(base, ".sdk-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	for _, p := range paths {
		b, _ := fs.ReadFile(fsys, p)
		dst := filepath.Join(tmp, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return "", err
		}
	}
	os.Chmod(tmp, 0o755)
	return dir, os.Rename(tmp, dir)
}
