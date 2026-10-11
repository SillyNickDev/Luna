package capsule

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

// A shared runtime is a pinned interpreter plus the SDK's dependencies, installed once
// under runtimes/<name>-<version>/ and mounted read-only into every capsule using it.
type download struct{ URL, SHA256 string }

type runtimeSpec struct {
	Version string
	Python  download
	Wheels  []download
}

// Pinned builds; every download is checked against its SHA-256 before use.
var pythonRuntimes = map[string]map[string]runtimeSpec{
	"python-3.12": {
		"amd64": {
			Version: "3.12.14",
			Python: download{"https://github.com/astral-sh/python-build-standalone/releases/download/20260924/cpython-3.12.14%2B20260924-x86_64-unknown-linux-gnu-install_only.tar.gz",
				"5eae8cf79dd47fc2496a4fccc892936be831ce7a84d984b2299dfb1cdb592682"},
			Wheels: []download{
				{"https://files.pythonhosted.org/packages/a0/bf/258cd7c0a7ed92745dc93c31666d462d05b702807a689744bd49fb833bde/grpcio-1.84.0-cp312-cp312-manylinux2014_x86_64.manylinux_2_17_x86_64.whl",
					"b44f0a0fc7bc6677d38cc80bca1a32814ce6c8f200fb8b3c1a61c9d77eaefbf3"},
				{"https://files.pythonhosted.org/packages/db/f3/3996583dd2906297a637af12114deddf7658af6e683fedb83be061983fb5/protobuf-7.36.2-cp310-abi3-manylinux2014_x86_64.whl",
					"89f23aa53c24553a2416fd4fd1ec06f74fa42b14b546d8883128813f775bbfd2"},
				typingExtensions,
			},
		},
		"arm64": {
			Version: "3.12.14",
			Python: download{"https://github.com/astral-sh/python-build-standalone/releases/download/20260924/cpython-3.12.14%2B20260924-aarch64-unknown-linux-gnu-install_only.tar.gz",
				"c0574c9c9fec561eb192ad510e4e79aaad039929b3531b3bb873e2f630fb14a9"},
			Wheels: []download{
				{"https://files.pythonhosted.org/packages/db/d1/a7b7cda98fcab9b3d2916204a872d87371158a7a34e41768f524584fb64d/grpcio-1.84.0-cp312-cp312-manylinux2014_aarch64.manylinux_2_17_aarch64.whl",
					"fbdbcd06986ede3ce584083b1dc2afe6808e8943e5cf50ad11183c03aceda25a"},
				{"https://files.pythonhosted.org/packages/b6/ea/91fdf7c2b8bbd49cde056f00a9df6773532987e1c00fe2830b895af95c7e/protobuf-7.36.2-cp310-abi3-manylinux2014_aarch64.whl",
					"e11e1f0180583a2af89db6a2ecd9e8dc40aa6d2988ca175bfd0e6d12ea72d74e"},
				typingExtensions,
			},
		},
	},
}

var typingExtensions = download{"https://files.pythonhosted.org/packages/18/67/36e9267722cc04a6b9f15c7f3441c2363321a3ea07da7ae0c0707beb2a9c/typing_extensions-4.15.0-py3-none-any.whl",
	"f0fa19c6845758ab08074a0cfa8b7aecb71c999ca73d62883bc25cc018c4e548"}

// RuntimeInfo is runtimes/<dir>/runtime.toml.
type RuntimeInfo struct {
	Name        string   `toml:"name"`
	Version     string   `toml:"version"`
	Interpreter string   `toml:"interpreter"` // inside the runtime dir
	Args        []string `toml:"args"`
	Loader      string   `toml:"loader"` // the interpreter's PT_INTERP; sysroot/ is mounted at its folder
	SHA256      string   `toml:"sha256"` // of every file except runtime.toml
	Dir         string   `toml:"-"`
}

// Paths skipped when unpacking the Python build: never imported by a plugin.
// The interpreter links libpython statically, so the shared copies go too.
var pythonSkip = []string{"python/share/", "python/include/", "python/lib/tcl", "python/lib/tk", "python/lib/itcl",
	"python/lib/thread", "python/lib/libtcl", "python/lib/libpython", "python/lib/libtk", "python/lib/pkgconfig/",
	"python/lib/python3.12/test/", "python/lib/python3.12/idlelib/", "python/lib/python3.12/tkinter/",
	"python/lib/python3.12/ensurepip/", "python/lib/python3.12/lib2to3/", "python/lib/python3.12/config-"}

// AddRuntime downloads, verifies and installs a shared runtime into dir/<name>-<version>.
func AddRuntime(dir, name string, progress io.Writer) (*RuntimeInfo, error) {
	specs, ok := pythonRuntimes[name]
	if !ok {
		return nil, fmt.Errorf("unknown runtime %q (available: python-3.12)", name)
	}
	spec, ok := specs[runtime.GOARCH]
	if !ok {
		return nil, fmt.Errorf("%s has no build for %s", name, runtime.GOARCH)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	final := filepath.Join(dir, name[:strings.IndexByte(name, '-')]+"-"+spec.Version)
	if _, err := os.Stat(final); err == nil {
		return nil, fmt.Errorf("%s is already installed", filepath.Base(final))
	}
	tmp, err := os.MkdirTemp(dir, ".add-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	dl := filepath.Join(tmp, "download")

	fmt.Fprintf(progress, "downloading Python %s\n", spec.Version)
	if err := fetch(spec.Python, dl); err != nil {
		return nil, err
	}
	if err := untarPython(dl, tmp); err != nil {
		return nil, err
	}
	site := filepath.Join(tmp, "site")
	for _, w := range spec.Wheels {
		fmt.Fprintf(progress, "downloading %s\n", path.Base(w.URL))
		if err := fetch(w, dl); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(site, 0o755); err != nil {
			return nil, err
		}
		if err := unzip(dl, site); err != nil {
			return nil, fmt.Errorf("%s: %w", path.Base(w.URL), err)
		}
	}
	os.Remove(dl)

	interp := "python/bin/python3.12"
	fmt.Fprintln(progress, "copying the C library the interpreter needs")
	loader, err := buildSysroot(tmp, interp, progress)
	if err != nil {
		return nil, err
	}
	// Capsules mount the runtime read-only, so compile bytecode now or every start pays for it.
	fmt.Fprintln(progress, "precompiling")
	cc := exec.Command(filepath.Join(tmp, interp), "-I", "-m", "compileall", "-q", "-j0",
		filepath.Join(tmp, "python/lib/python3.12"), site)
	cc.Env = []string{}
	if out, err := cc.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("precompiling: %v\n%s", err, out)
	}

	info := &RuntimeInfo{Name: name, Version: spec.Version, Interpreter: interp, Args: []string{"-I", "-S"}, Loader: loader}
	if info.SHA256, err = treeHash(tmp); err != nil {
		return nil, err
	}
	f, err := os.Create(filepath.Join(tmp, "runtime.toml"))
	if err != nil {
		return nil, err
	}
	if err := toml.NewEncoder(f).Encode(info); err != nil {
		return nil, err
	}
	f.Close()
	if err := os.Rename(tmp, final); err != nil {
		return nil, err
	}
	info.Dir = final
	return info, nil
}

// fetch downloads d.URL to dst and checks its SHA-256.
func fetch(d download, dst string) error {
	c := &http.Client{Timeout: 10 * time.Minute}
	resp, err := c.Get(d.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", d.URL, resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, 256<<20)); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != d.SHA256 {
		return fmt.Errorf("%s: SHA-256 mismatch (got %s), refusing it", path.Base(d.URL), got)
	}
	return f.Close()
}

// untarPython extracts the regular files of a python-build-standalone archive into dst.
// Links are skipped (nothing Luna runs needs them), and every name must stay inside dst.
func untarPython(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(h.Name, "/")
		if !localPath(name) {
			return fmt.Errorf("archive entry %q escapes the runtime", h.Name)
		}
		if slices.ContainsFunc(pythonSkip, func(p string) bool { return strings.HasPrefix(name+"/", p) || strings.HasPrefix(name, p) }) {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
				return err
			}
			w, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(h.Mode)&0o755|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(w, io.LimitReader(tr, 512<<20))
			w.Close()
			if err != nil {
				return err
			}
		}
	}
}

// libDirs is where the host's shared libraries live: the usual folders plus whatever
// /etc/ld.so.conf lists (Gentoo keeps libstdc++ in a GCC-versioned folder there).
func libDirs() []string {
	dirs := []string{"/lib64", "/usr/lib64", "/lib/x86_64-linux-gnu", "/usr/lib/x86_64-linux-gnu",
		"/lib/aarch64-linux-gnu", "/usr/lib/aarch64-linux-gnu", "/lib", "/usr/lib"}
	var read func(path string, depth int)
	read = func(path string, depth int) {
		b, err := os.ReadFile(path)
		if err != nil || depth > 3 {
			return
		}
		for l := range strings.Lines(string(b)) {
			l = strings.TrimSpace(l)
			if inc, ok := strings.CutPrefix(l, "include "); ok {
				if !filepath.IsAbs(inc) {
					inc = filepath.Join(filepath.Dir(path), inc)
				}
				matches, _ := filepath.Glob(strings.TrimSpace(inc))
				for _, m := range matches {
					read(m, depth+1)
				}
			} else if filepath.IsAbs(l) {
				dirs = append(dirs, l)
			}
		}
	}
	read("/etc/ld.so.conf", 0)
	return dirs
}

// buildSysroot copies the host libraries the interpreter and its extension modules need
// into dir/sysroot, so capsules never see the host's own library folders. It returns the loader path.
func buildSysroot(dir, interp string, progress io.Writer) (string, error) {
	sys := filepath.Join(dir, "sysroot")
	if err := os.MkdirAll(sys, 0o755); err != nil {
		return "", err
	}
	main, err := elf.Open(filepath.Join(dir, interp))
	if err != nil {
		return "", err
	}
	var loader string
	for _, p := range main.Progs {
		if p.Type == elf.PT_INTERP {
			b, _ := io.ReadAll(p.Open())
			loader = strings.TrimRight(string(b), "\x00")
		}
	}
	main.Close()
	if loader == "" || !filepath.IsAbs(loader) {
		return "", errors.New("interpreter has no ELF loader")
	}
	bundled := map[string]bool{} // libraries the runtime ships itself
	var queue []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && (strings.HasSuffix(p, ".so") || strings.Contains(p, ".so.") || p == filepath.Join(dir, interp)) {
			bundled[filepath.Base(p)] = true
			queue = append(queue, p)
		}
		return nil
	})
	copied := map[string]bool{}
	dirs := libDirs()
	copyLib := func(src, name string) error {
		b, err := os.ReadFile(src) // follows the host's symlinks to the real file
		if err != nil {
			return err
		}
		copied[name] = true
		return os.WriteFile(filepath.Join(sys, name), b, 0o755)
	}
	if err := copyLib(loader, filepath.Base(loader)); err != nil {
		return "", fmt.Errorf("copying the loader: %w", err)
	}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		f, err := elf.Open(p)
		if err != nil {
			continue // not ELF (a linker script, or data)
		}
		libs, _ := f.ImportedLibraries()
		f.Close()
		for _, lib := range libs {
			if bundled[lib] || copied[lib] || strings.Contains(lib, "$ORIGIN") {
				continue
			}
			found := ""
			for _, d := range dirs {
				if _, err := os.Stat(filepath.Join(d, lib)); err == nil {
					found = filepath.Join(d, lib)
					break
				}
			}
			if found == "" && strings.Contains(p, "/lib-dynload/") {
				// An optional stdlib module (tkinter, curses) whose library this host lacks: drop it.
				os.Remove(p)
				fmt.Fprintf(progress, "  left out %s (needs %s)\n", filepath.Base(p), lib)
				break
			}
			if found == "" {
				return "", fmt.Errorf("%s needs %s, which isn't on this system", filepath.Base(p), lib)
			}
			if err := copyLib(found, lib); err != nil {
				return "", err
			}
			queue = append(queue, filepath.Join(sys, lib))
		}
	}
	return loader, nil
}

func treeHash(dir string) (string, error) {
	lock, err := hashTreeExcept(dir, "runtime.toml")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(lock)
	return hex.EncodeToString(sum[:]), nil
}

var verified sync.Map // runtime dir → error, checked once per daemon run

// ResolveRuntime finds the newest installed runtime matching name ("python-3.12" matches 3.12.x)
// and verifies its files the first time it's used.
func ResolveRuntime(dir, name string) (*RuntimeInfo, error) {
	family, want, _ := strings.Cut(name, "-")
	ents, _ := os.ReadDir(dir)
	var best *RuntimeInfo
	var bestV []int
	for _, e := range ents {
		v, ok := strings.CutPrefix(e.Name(), family+"-")
		if !ok || !e.IsDir() || (v != want && !strings.HasPrefix(v, want+".")) {
			continue
		}
		var info RuntimeInfo
		if _, err := toml.DecodeFile(filepath.Join(dir, e.Name(), "runtime.toml"), &info); err != nil {
			continue
		}
		info.Dir = filepath.Join(dir, e.Name())
		if nv := versionParts(info.Version); best == nil || slices.Compare(nv, bestV) > 0 {
			best, bestV = &info, nv
		}
	}
	if best == nil {
		return nil, fmt.Errorf("runtime_missing: %s isn't installed; run: luna capsule runtime add %s", name, name)
	}
	errv, ok := verified.Load(best.Dir)
	if !ok {
		errv = error(nil)
		if got, err := treeHash(best.Dir); err != nil {
			errv = err
		} else if got != best.SHA256 {
			errv = fmt.Errorf("runtime %s was modified on disk; remove it and run luna capsule runtime add %s", filepath.Base(best.Dir), name)
		}
		verified.Store(best.Dir, errv)
	}
	if err, _ := errv.(error); err != nil {
		return nil, err
	}
	return best, nil
}

func versionParts(v string) []int {
	var out []int
	for _, p := range strings.Split(v, ".") {
		n, _ := strconv.Atoi(p)
		out = append(out, n)
	}
	return out
}
