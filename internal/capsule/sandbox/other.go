//go:build !linux && !windows

package sandbox

import "errors"

// shortcut: Linux and Windows backends exist; macOS (Seatbelt) comes after v1.
var errUnsupported = errors.New("capsules need the Linux or Windows backend for now")

type Proc struct{}

func Start(Spec) (*Proc, error)     { return nil, errUnsupported }
func Init()                         {}
func Doctor() []Check               { return []Check{{Name: "backend", Detail: errUnsupported.Error()}} }
func (*Proc) Pid() int              { return 0 }
func (*Proc) Done() <-chan struct{} { return nil }
func (*Proc) Exit() Exit            { return Exit{} }
func (*Proc) Kill()                 {}
