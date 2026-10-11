package capsule

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"luna/internal/capsule/sandbox"
)

// Describe runs a package folder's entry once with LUNA_DESCRIBE=1, sandboxed like any
// capsule, and returns what it printed: the SDK's description of its handlers and commands.
func Describe(dir, runDir, runtimesDir string, sdkDirs map[string]string) ([]byte, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	pkg := &Unpacked{Dir: dir, Manifest: m}
	ls, err := launch(pkg, runtimesDir)
	if err != nil {
		return nil, err
	}
	family, _, _ := strings.Cut(m.Runtime, "-")
	if sdkDirs[family] == "" {
		return nil, fmt.Errorf("no SDK can describe %s plugins", m.Runtime)
	}
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp(runDir, "describe-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	var out, errOut limitedBuffer
	p, err := sandbox.Start(sandbox.Spec{
		Name:   m.ID + "-describe",
		Root:   root,
		Mounts: append([]sandbox.Mount{{Src: dir, Dst: "/pkg"}, {Src: sdkDirs[family], Dst: "/sdk"}}, ls.mounts...),
		Argv:   ls.argv,
		Env:    append([]string{"LUNA_DESCRIBE=1", "HOME=/pkg"}, ls.env...),
		Dir:    "/pkg",
		Memory: int64(m.Limits.Memory), CPU: 1, Pids: m.Limits.Pids, Files: m.Limits.Files,
		Stdout: &out, Stderr: &errOut,
	})
	if err != nil {
		return nil, err
	}
	select {
	case <-p.Done():
	case <-time.After(15 * time.Second):
		p.Kill()
		<-p.Done()
		return nil, errors.New("the plugin took over 15 s to load")
	}
	if ex := p.Exit(); ex.Cause != "" {
		return nil, fmt.Errorf("the plugin failed to load (%s):\n%s", ex.Cause, strings.TrimSpace(errOut.String()))
	}
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	return lines[len(lines)-1], nil // the description is the last line; prints before it are the plugin's own
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1<<20 {
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
