package capsule

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

type nodeSpec struct {
	Version string
	Archive download
}

// Pinned Node.js builds from nodejs.org; 22.18+ runs .ts files directly (type stripping).
var nodeRuntimes = map[string]map[string]nodeSpec{
	"node-22": {
		"amd64": {"22.23.3", download{"https://nodejs.org/dist/v22.23.3/node-v22.23.3-linux-x64.tar.gz",
			"1084aa36196bba4c3a5e69a1ee388a6e4ff729dad09445fbcd434b28fe3c24af"}},
		"arm64": {"22.23.3", download{"https://nodejs.org/dist/v22.23.3/node-v22.23.3-linux-arm64.tar.gz",
			"5ced2d48d1d7198739b7f86804de0171aefb6823b684b12341d3321afc3cb0b2"}},
	},
}

// addNode installs just the node binary and the C libraries it links: no npm, no headers.
func addNode(dir, name string, progress io.Writer) (*RuntimeInfo, error) {
	spec, ok := nodeRuntimes[name][runtime.GOARCH]
	if !ok {
		return nil, fmt.Errorf("%s has no build for %s", name, runtime.GOARCH)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	final := filepath.Join(dir, "node-"+spec.Version)
	if _, err := os.Stat(final); err == nil {
		return nil, fmt.Errorf("%s is already installed", filepath.Base(final))
	}
	tmp, err := os.MkdirTemp(dir, ".add-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	dl := filepath.Join(tmp, "download")
	fmt.Fprintf(progress, "downloading Node.js %s\n", spec.Version)
	if err := fetch(spec.Archive, dl); err != nil {
		return nil, err
	}
	err = untar(dl, tmp, func(name string) (string, bool) {
		_, rest, _ := strings.Cut(name, "/") // node-v22.x-linux-x64/bin/node
		return "node/bin/node", rest == "bin/node"
	})
	if err != nil {
		return nil, err
	}
	os.Remove(dl)
	interp := path.Join("node", "bin", "node")
	if _, err := os.Stat(filepath.Join(tmp, interp)); err != nil {
		return nil, fmt.Errorf("the Node.js archive has no bin/node")
	}
	fmt.Fprintln(progress, "copying the C libraries node needs")
	loader, err := buildSysroot(tmp, interp, progress)
	if err != nil {
		return nil, err
	}
	// No __proto__ (a prototype-pollution gadget) and no native addons; the SDK needs neither.
	// A small young generation and one V8 worker thread keep an idle capsule near 24 MB, not 42.
	return finishRuntime(tmp, final, &RuntimeInfo{Name: name, Version: spec.Version, Interpreter: interp,
		Args:   []string{"--disable-proto=delete", "--no-addons", "--max-old-space-size=48", "--max-semi-space-size=1", "--v8-pool-size=1"},
		Loader: loader})
}
