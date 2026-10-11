//go:build windows

package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows backend (Capsules spec, Platform backends): each capsule runs in its own
// AppContainer with no capabilities, so it can't reach the user's files, the registry or
// the network; a Job Object caps memory, CPU and processes and kills the tree with it.
// There's no mount namespace, so Mounts become ACL grants for the container's SID and the
// capsule's paths (/pkg, /data, /run/capsule) are rewritten to the real folders.

var (
	userenv                     = windows.NewLazySystemDLL("userenv.dll")
	createAppContainerProfile   = userenv.NewProc("CreateAppContainerProfile")
	deriveAppContainerSidByName = userenv.NewProc("DeriveAppContainerSidFromAppContainerName")
)

const (
	procThreadAttributeSecurityCapabilities = 0x00020009
	procThreadAttributeChildProcessPolicy   = 0x0002000E
	procThreadAttributeMitigationPolicy     = 0x00020007
	childProcessRestricted                  = 0x01
	// Mitigations: no legacy extension points (AppInit DLLs, IMEs) and no DLLs from remote shares.
	mitigationExtensionPointDisable = 0x1 << 32
	mitigationImageLoadNoRemote     = 0x1 << 52
	jobObjectMsgEndOfJobTime        = 1
	jobObjectMsgProcessMemoryLimit  = 9
	jobObjectMsgJobMemoryLimit      = 10
	cpuRateEnable                   = 0x1
	cpuRateHardCap                  = 0x4
	hresultAlreadyExists            = 0x800700B7
)

type securityCapabilities struct {
	AppContainerSid *windows.SID
	Capabilities    *windows.SIDAndAttributes
	CapabilityCount uint32
	Reserved        uint32
}

type cpuRateControl struct {
	ControlFlags uint32
	CPURate      uint32 // in 1/100ths of a percent of the whole machine
}

type associateCompletionPort struct {
	CompletionKey  uintptr
	CompletionPort windows.Handle
}

// Proc is a running capsule: its process inside a job that dies with it.
type Proc struct {
	process windows.Handle
	job     windows.Handle
	port    windows.Handle
	pid     uint32
	done    chan struct{}
	exit    Exit
	oom     atomic.Bool
	killMu  sync.Mutex
}

// containerName is the AppContainer profile for a capsule: one per capsule, stable across runs.
func containerName(capsule string) string {
	n := "Luna.Capsule." + capsule
	if len(n) > 64 {
		h := sha256.Sum256([]byte(capsule))
		n = "Luna.Capsule." + hex.EncodeToString(h[:8])
	}
	return n
}

// containerSID creates the capsule's AppContainer profile if needed and returns its SID.
func containerSID(capsule string) (*windows.SID, error) {
	if err := userenv.Load(); err != nil {
		return nil, fmt.Errorf("AppContainers need Windows 10 1809 or newer: %w", err)
	}
	name, _ := windows.UTF16PtrFromString(containerName(capsule))
	var sid *windows.SID
	hr, _, _ := createAppContainerProfile.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(name)),
		uintptr(unsafe.Pointer(name)), 0, 0, uintptr(unsafe.Pointer(&sid)))
	if uint32(hr) == hresultAlreadyExists {
		hr, _, _ = deriveAppContainerSidByName.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&sid)))
	}
	if hr != 0 {
		return nil, fmt.Errorf("AppContainer profile %s: HRESULT 0x%08X", containerName(capsule), uint32(hr))
	}
	// The SID was allocated by the OS; copy it so FreeSid can't pull it out from under us.
	cp, err := sid.Copy()
	windows.FreeSid(sid)
	return cp, err
}

// grant gives the container's SID access to path: read and execute, or read and write
// without execute (like Linux's noexec data/), inherited by everything below a folder.
func grant(sid *windows.SID, path string, rw bool) error {
	access := windows.ACCESS_MASK(windows.GENERIC_READ | windows.GENERIC_EXECUTE)
	if rw {
		access = windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.DELETE
	}
	inherit := uint32(windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)
	if fi, err := os.Stat(path); err != nil {
		return err
	} else if !fi.IsDir() {
		inherit = windows.NO_INHERITANCE
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	old, _, err := sd.DACL()
	if err != nil {
		return err
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: access, AccessMode: windows.GRANT_ACCESS, Inheritance: inherit,
		Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(sid)},
	}}, old)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

func Start(s Spec) (*Proc, error) {
	sid, err := containerSID(s.Name)
	if err != nil {
		return nil, err
	}
	for _, m := range s.Mounts {
		if err := grant(sid, m.Src, m.RW); err != nil {
			return nil, fmt.Errorf("granting the capsule %s: %w", m.Dst, err)
		}
	}
	argv := slices.Clone(s.Argv)
	for i, a := range argv {
		argv[i], _ = hostPath(s.Mounts, a)
	}
	env := []string{"SystemRoot=" + os.Getenv("SystemRoot")} // Windows' own DLLs need it
	for _, kv := range s.Env {
		k, v, _ := strings.Cut(kv, "=")
		v, _ = hostPath(s.Mounts, v)
		env = append(env, k+"="+v)
	}
	dir, _ := hostPath(s.Mounts, s.Dir)

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	p := &Proc{job: job, done: make(chan struct{})}
	ok := false
	defer func() {
		if !ok {
			p.close()
		}
	}()
	if err := p.limit(s); err != nil {
		return nil, fmt.Errorf("job limits: %w", err)
	}

	stdout, outW, err := pipe()
	if err != nil {
		return nil, err
	}
	stderr, errW, err := pipe()
	if err != nil {
		return nil, err
	}
	attrs, err := windows.NewProcThreadAttributeList(4)
	if err != nil {
		return nil, err
	}
	defer attrs.Delete()
	caps := securityCapabilities{AppContainerSid: sid} // no capabilities: no network, no libraries, no devices
	handles := []windows.Handle{outW, errW}
	childPolicy := uint32(childProcessRestricted)
	mitigations := uint64(mitigationExtensionPointDisable | mitigationImageLoadNoRemote)
	for _, a := range []struct {
		attr uintptr
		val  unsafe.Pointer
		size uintptr
	}{
		{procThreadAttributeSecurityCapabilities, unsafe.Pointer(&caps), unsafe.Sizeof(caps)},
		{windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles)) * unsafe.Sizeof(handles[0])},
		{procThreadAttributeChildProcessPolicy, unsafe.Pointer(&childPolicy), unsafe.Sizeof(childPolicy)},
		{procThreadAttributeMitigationPolicy, unsafe.Pointer(&mitigations), unsafe.Sizeof(mitigations)},
	} {
		if err := attrs.Update(a.attr, a.val, a.size); err != nil {
			return nil, fmt.Errorf("process attribute 0x%x: %w", a.attr, err)
		}
	}
	si := &windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdOutput, si.StdErr = outW, errW // no stdin: capsules talk over their sockets

	cmdLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(argv))
	if err != nil {
		return nil, err
	}
	app, err := windows.UTF16PtrFromString(argv[0])
	if err != nil {
		return nil, err
	}
	wdir, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return nil, err
	}
	block, err := envBlock(env)
	if err != nil {
		return nil, err
	}
	var pi windows.ProcessInformation
	err = windows.CreateProcess(app, cmdLine, nil, nil, true,
		windows.CREATE_SUSPENDED|windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW|windows.EXTENDED_STARTUPINFO_PRESENT,
		&block[0], wdir, &si.StartupInfo, &pi)
	windows.CloseHandle(outW)
	windows.CloseHandle(errW)
	if err != nil {
		windows.CloseHandle(stdout)
		windows.CloseHandle(stderr)
		return nil, fmt.Errorf("starting the capsule in its AppContainer: %w", err)
	}
	p.process, p.pid = pi.Process, pi.ProcessId
	// Into the job before its first instruction, so nothing it starts escapes the limits.
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Thread)
		return nil, fmt.Errorf("job object: %w", err)
	}
	windows.ResumeThread(pi.Thread)
	windows.CloseHandle(pi.Thread)
	ok = true
	var copies sync.WaitGroup
	for _, c := range []struct {
		h windows.Handle
		w io.Writer
	}{{stdout, s.Stdout}, {stderr, s.Stderr}} {
		copies.Add(1)
		go func() {
			defer copies.Done()
			f := os.NewFile(uintptr(c.h), "capsule output")
			defer f.Close()
			if c.w == nil {
				c.w = io.Discard
			}
			io.Copy(c.w, f)
		}()
	}
	go p.watchJob()
	go p.wait(&copies)
	return p, nil
}

func pipe() (r, w windows.Handle, err error) {
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	if err := windows.CreatePipe(&r, &w, sa, 0); err != nil {
		return 0, 0, err
	}
	windows.SetHandleInformation(r, windows.HANDLE_FLAG_INHERIT, 0) // only the child's end is inherited
	return r, w, nil
}

func envBlock(env []string) ([]uint16, error) {
	var b []uint16
	for _, kv := range env {
		u, err := windows.UTF16FromString(kv)
		if err != nil {
			return nil, err
		}
		b = append(b, u...)
	}
	return append(b, 0), nil
}

// limit puts the spec's caps on the job, plus UI restrictions (clipboard, global atoms,
// other windows' handles), and asks to hear when memory runs out.
func (p *Proc) limit(s Spec) error {
	var ext windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	ext.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION
	if s.Memory > 0 {
		ext.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		ext.JobMemoryLimit = uintptr(s.Memory)
	}
	if s.Pids > 0 {
		ext.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
		ext.BasicLimitInformation.ActiveProcessLimit = uint32(s.Pids)
	}
	if _, err := windows.SetInformationJobObject(p.job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&ext)), uint32(unsafe.Sizeof(ext))); err != nil {
		return err
	}
	ui := windows.JOBOBJECT_BASIC_UI_RESTRICTIONS{UIRestrictionsClass: windows.JOB_OBJECT_UILIMIT_DESKTOP |
		windows.JOB_OBJECT_UILIMIT_DISPLAYSETTINGS | windows.JOB_OBJECT_UILIMIT_EXITWINDOWS | windows.JOB_OBJECT_UILIMIT_GLOBALATOMS |
		windows.JOB_OBJECT_UILIMIT_HANDLES | windows.JOB_OBJECT_UILIMIT_READCLIPBOARD | windows.JOB_OBJECT_UILIMIT_SYSTEMPARAMETERS |
		windows.JOB_OBJECT_UILIMIT_WRITECLIPBOARD}
	if _, err := windows.SetInformationJobObject(p.job, windows.JobObjectBasicUIRestrictions,
		uintptr(unsafe.Pointer(&ui)), uint32(unsafe.Sizeof(ui))); err != nil {
		return err
	}
	if s.CPU > 0 && s.CPU < 1 {
		rate := cpuRateControl{ControlFlags: cpuRateEnable | cpuRateHardCap, CPURate: uint32(max(s.CPU, 0.01) * 10000)}
		if _, err := windows.SetInformationJobObject(p.job, windows.JobObjectCpuRateControlInformation,
			uintptr(unsafe.Pointer(&rate)), uint32(unsafe.Sizeof(rate))); err != nil {
			return err
		}
	}
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 1)
	if err != nil {
		return err
	}
	p.port = port
	assoc := associateCompletionPort{CompletionKey: uintptr(p.job), CompletionPort: port}
	_, err = windows.SetInformationJobObject(p.job, windows.JobObjectAssociateCompletionPortInformation,
		uintptr(unsafe.Pointer(&assoc)), uint32(unsafe.Sizeof(assoc)))
	return err
}

// watchJob ends the capsule when it hits its memory cap, as on Linux: killed, cause oom,
// rather than left to limp along with failing allocations.
func (p *Proc) watchJob() {
	for {
		var msg uint32
		var key uintptr
		var ov *windows.Overlapped
		if err := windows.GetQueuedCompletionStatus(p.port, &msg, &key, &ov, windows.INFINITE); err != nil {
			return // the port was closed: the capsule is gone
		}
		if msg == jobObjectMsgJobMemoryLimit || msg == jobObjectMsgProcessMemoryLimit {
			p.oom.Store(true)
			p.Kill()
		}
	}
}

func (p *Proc) wait(copies *sync.WaitGroup) {
	windows.WaitForSingleObject(p.process, windows.INFINITE)
	var code uint32
	windows.GetExitCodeProcess(p.process, &code)
	copies.Wait()
	switch {
	case p.oom.Load():
		p.exit = Exit{Cause: "oom"}
	case code == 0:
	case code >= 0xC0000000: // an NTSTATUS: access violation, stack overflow, ...
		p.exit = Exit{Cause: "crash", Signal: fmt.Sprintf("0x%08X", code)}
	default:
		p.exit = Exit{Cause: "exit", Code: int(code)}
	}
	p.close()
	close(p.done)
}

func (p *Proc) close() {
	p.killMu.Lock()
	defer p.killMu.Unlock()
	if p.job != 0 {
		windows.CloseHandle(p.job) // kill-on-close takes anything still in it
		p.job = 0
	}
	if p.port != 0 {
		windows.CloseHandle(p.port)
		p.port = 0
	}
	if p.process != 0 {
		windows.CloseHandle(p.process)
		p.process = 0
	}
}

func (p *Proc) Pid() int              { return int(p.pid) }
func (p *Proc) Done() <-chan struct{} { return p.done }
func (p *Proc) Exit() Exit            { return p.exit }

// Kill ends the capsule's whole process tree.
func (p *Proc) Kill() {
	p.killMu.Lock()
	defer p.killMu.Unlock()
	if p.job != 0 {
		windows.TerminateJobObject(p.job, 1)
	}
}

// Init is Linux's setup stage; Windows sets everything up before the process starts.
func Init() {}

// Doctor reports which isolation guarantees this machine can give.
func Doctor() []Check {
	v := windows.RtlGetVersion()
	ac := v.MajorVersion > 10 || v.MajorVersion == 10 && v.BuildNumber >= 17763
	detail := fmt.Sprintf("Windows %d.%d build %d: files, registry, network and other processes walled off", v.MajorVersion,
		v.MinorVersion, v.BuildNumber)
	if !ac {
		detail = "needs Windows 10 1809 (build 17763) or newer"
	} else if err := userenv.Load(); err != nil {
		ac, detail = false, err.Error()
	}
	return []Check{
		{"AppContainer", detail, ac},
		{"job object", "hard memory cap, CPU rate cap, process limit, whole tree killed on stop", true},
		{"child processes", "blocked: capsules can't start programs", true},
		{"other processes", "can't open, signal or debug them, but can list their names (Windows can't hide them)", false},
		{"open files cap", "not enforced on Windows", false},
		{"network allowlists", "only the hosts a capsule lists, through Luna's egress proxy; no private addresses", true},
		{"data quota", "not enforced on Windows yet", false},
	}
}
