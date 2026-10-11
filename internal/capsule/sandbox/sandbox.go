// Package sandbox starts one capsule process inside the OS's native isolation.
// It is the one package allowed to use unsafe (Security tab), for seccomp and Landlock.
package sandbox

import "io"

// InitArg is the hidden argv[1] that makes the luna binary act as a capsule's setup stage.
const InitArg = "__capsule-init"

// Mount exposes a host path inside the capsule; read-only unless RW.
type Mount struct {
	Src, Dst string
	RW       bool
}

type Spec struct {
	Name     string   // [a-z0-9.-], used in the cgroup unit name
	Root     string   // empty host dir the capsule's root is built on
	Mounts   []Mount  //
	Argv     []string // argv[0] is a path inside the capsule
	Env      []string
	Dir      string // working directory inside the capsule
	Memory   int64
	CPU      float64
	Pids     int
	Files    int
	FileSize int64     // per-file write cap (data quota stand-in)
	Stdout   io.Writer `json:"-"`
	Stderr   io.Writer `json:"-"`
}

// Exit says why a capsule process ended. Cause is "" for a clean exit (code 0),
// otherwise one of the spec's failure causes: exit, crash, oom, policy.
type Exit struct {
	Cause  string
	Code   int
	Signal string
}

// Check is one line of luna capsule doctor.
type Check struct {
	Name, Detail string
	OK           bool
}
