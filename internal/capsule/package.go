package capsule

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	LockFile = "capsule.lock"
	SigFile  = "capsule.sig"

	maxFiles = 10000
	maxBytes = 512 << 20
)

// hashTree returns "sha256  path" lines for every file under root except the lock
// and signature. Symlinks and special files are refused, so a package can't point outside itself.
func hashTree(root string) ([]byte, error) {
	var lines []string
	var total int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() || rel == LockFile || rel == SigFile {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: only regular files are allowed in a package", rel)
		}
		if len(lines) == maxFiles {
			return fmt.Errorf("package has more than %d files", maxFiles)
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if total += n; err == nil && total > maxBytes {
			err = fmt.Errorf("package is larger than %d MiB", maxBytes>>20)
		}
		if err != nil {
			return err
		}
		lines = append(lines, hex.EncodeToString(h.Sum(nil))+"  "+rel+"\n")
		return nil
	})
	slices.SortFunc(lines, func(a, b string) int { return strings.Compare(a[66:], b[66:]) })
	return []byte(strings.Join(lines, "")), err
}

// WriteLock (re)generates dir/capsule.lock.
func WriteLock(dir string) error {
	lock, err := hashTree(dir)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, LockFile), lock, 0o644)
}

// Verify checks that dir holds exactly the files in its capsule.lock, with matching hashes,
// and returns the package hash (SHA-256 of the lock) that content-addresses it.
func Verify(dir string) (string, error) {
	want, err := os.ReadFile(filepath.Join(dir, LockFile))
	if err != nil {
		return "", errors.New("package has no capsule.lock (run luna capsule pack)")
	}
	got, err := hashTree(dir)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(want, got) {
		return "", fmt.Errorf("package doesn't match its capsule.lock: %s", lockDiff(want, got))
	}
	sum := sha256.Sum256(want)
	return hex.EncodeToString(sum[:]), nil
}

func lockDiff(want, got []byte) string {
	lines := func(b []byte) map[string]string {
		m := map[string]string{}
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			if h, p, ok := strings.Cut(sc.Text(), "  "); ok {
				m[p] = h
			}
		}
		return m
	}
	w, g := lines(want), lines(got)
	for p, h := range w {
		if gh, ok := g[p]; !ok {
			return p + " is missing"
		} else if gh != h {
			return p + " was modified"
		}
	}
	for p := range g {
		if _, ok := w[p]; !ok {
			return p + " isn't in the lock"
		}
	}
	return "lock file is malformed"
}

// Pack writes dir's lock and zips the package to out (a .capsule file).
func Pack(dir, out string) error {
	if _, err := LoadManifest(dir); err != nil {
		return err
	}
	if err := WriteLock(dir); err != nil {
		return err
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	if err := zw.AddFS(os.DirFS(dir)); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return f.Close()
}

// unzip extracts a .capsule archive into the empty dir dst. Every name must stay inside dst;
// absolute paths, "..", symlinks and other special entries abort the whole extraction.
func unzip(src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	if len(zr.File) > maxFiles {
		return fmt.Errorf("archive has more than %d entries", maxFiles)
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer root.Close()
	var total int64
	for _, zf := range zr.File {
		name := strings.TrimSuffix(zf.Name, "/")
		if !localPath(name) {
			return fmt.Errorf("archive entry %q escapes the package", zf.Name)
		}
		mode := zf.Mode()
		if mode.IsDir() {
			if err := root.MkdirAll(name, 0o755); err != nil {
				return err
			}
			continue
		}
		if !mode.IsRegular() {
			return fmt.Errorf("archive entry %q isn't a regular file", zf.Name)
		}
		if dir := filepath.Dir(name); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		if err := func() error {
			r, err := zf.Open()
			if err != nil {
				return err
			}
			defer r.Close()
			w, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm()&0o755|0o600)
			if err != nil {
				return err
			}
			defer w.Close()
			n, err := io.Copy(w, io.LimitReader(r, maxBytes-total+1))
			if total += n; err == nil && total > maxBytes {
				err = fmt.Errorf("archive unpacks to more than %d MiB", maxBytes>>20)
			}
			if err != nil {
				return err
			}
			return w.Close()
		}(); err != nil {
			return err
		}
	}
	return nil
}

// copyTree copies a package folder into the empty dir dst, refusing symlinks.
func copyTree(src, dst string) error {
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer root.Close()
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		switch {
		case rel == ".":
			return nil
		case d.IsDir():
			return root.Mkdir(rel, 0o755)
		case !d.Type().IsRegular():
			return fmt.Errorf("%s: only regular files are allowed in a package", rel)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm()&0o755|0o600)
		if err != nil {
			return err
		}
		defer out.Close()
		if _, err := io.Copy(out, in); err != nil {
			return err
		}
		return out.Close()
	})
}

// Unpacked is a verified package in the content-addressed store.
type Unpacked struct {
	Dir      string
	Hash     string
	Manifest *Manifest
}

// AddToStore verifies a package folder or .capsule file and places it under storeDir/<hash>.
// shortcut: capsule.sig isn't checked yet; add once signing keys and the index exist (open question).
func AddToStore(storeDir, src string) (*Unpacked, error) {
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return nil, err
	}
	fi, err := os.Stat(src)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(storeDir, ".unpack-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if fi.IsDir() {
		err = copyTree(src, tmp)
	} else {
		err = unzip(src, tmp)
	}
	if err != nil {
		return nil, err
	}
	hash, err := Verify(tmp)
	if err != nil {
		return nil, err
	}
	m, err := LoadManifest(tmp)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(storeDir, hash)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(tmp, dir); err != nil {
			return nil, err
		}
	}
	return &Unpacked{Dir: dir, Hash: hash, Manifest: m}, nil
}

// FromStore re-verifies a package already in the store, so tampering on disk is caught at every start.
func FromStore(storeDir, hash string) (*Unpacked, error) {
	if b, err := hex.DecodeString(hash); err != nil || len(b) != sha256.Size {
		return nil, fmt.Errorf("bad package hash %q", hash)
	}
	dir := filepath.Join(storeDir, hash)
	got, err := Verify(dir)
	if err != nil {
		return nil, err
	}
	if got != hash {
		return nil, fmt.Errorf("package %s.. doesn't match its store address", hash[:12])
	}
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	return &Unpacked{Dir: dir, Hash: hash, Manifest: m}, nil
}
