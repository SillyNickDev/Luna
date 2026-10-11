package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"luna/internal/capsule"
	"luna/internal/capsule/sandbox"
	"luna/internal/event"
	"luna/internal/store"
)

func (p paths) capsuleStore() string { return filepath.Join(p.data, "capsules", "store") }
func (p paths) capsuleData() string  { return filepath.Join(p.data, "capsules", "data") }

// runDir holds capsule sockets and roots. It's per data dir, so two Luna instances can't
// collide, and short, because Unix socket paths max out at 108 bytes.
func (p paths) runDir() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = filepath.Join(p.data, "run")
	}
	sum := sha256.Sum256([]byte(p.data))
	return filepath.Join(base, "luna-"+hex.EncodeToString(sum[:4]))
}

// approval describes pkg's permissions in plain words. The fingerprint covers the
// permissions only, so an update that doesn't widen them needs no new approval.
func approval(m *capsule.Manifest) (text, fingerprint string) {
	var b strings.Builder
	fmt.Fprintf(&b, "  memory up to %d MiB, %g%% of one CPU, %d processes, %d open files\n",
		m.Limits.Memory>>20, m.Limits.CPU*100, m.Limits.Pids, m.Limits.Files)
	if m.FS.Writable == "data" {
		fmt.Fprintf(&b, "  writes only to its own data folder (files up to %d MiB)\n", m.FS.DataQuota>>20)
	} else {
		b.WriteString("  can't write any files\n")
	}
	if len(m.Net) == 0 {
		b.WriteString("  no network\n")
	} else {
		fmt.Fprintf(&b, "  network: %s\n", strings.Join(m.Net, ", "))
	}
	if len(m.Devices) == 0 {
		b.WriteString("  no devices\n")
	} else {
		fmt.Fprintf(&b, "  devices: %s\n", strings.Join(m.Devices, ", "))
	}
	for _, u := range m.Links.Uses {
		fmt.Fprintf(&b, "  calls %s\n", u)
	}
	for _, l := range m.Links.Provides {
		fmt.Fprintf(&b, "  offers the link %q to other plugins\n", l)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return b.String(), hex.EncodeToString(sum[:])
}

// capsuleCLI runs the subcommands that work without the daemon, and forwards the rest.
func capsuleCLI(p paths, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: luna capsule install|uninstall|list|enable|disable|restart|logs|inspect|doctor|pack|verify")
	}
	switch args[0] {
	case "install":
		return installCapsule(p, args[1:])
	case "doctor":
		for _, c := range sandbox.Doctor() {
			mark := "✓"
			if !c.OK {
				mark = "✗"
			}
			fmt.Printf("%s %-18s %s\n", mark, c.Name, c.Detail)
		}
		return nil
	case "pack":
		fs := flag.NewFlagSet("pack", flag.ContinueOnError)
		out := fs.String("o", "", "output .capsule file")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		dir := fs.Arg(0)
		if fs.NArg() > 1 { // flags may also follow the folder
			if err := fs.Parse(fs.Args()[1:]); err != nil {
				return err
			}
		}
		if dir == "" {
			dir = "."
		}
		m, err := capsule.LoadManifest(dir)
		if err != nil {
			return err
		}
		if *out == "" {
			*out = fmt.Sprintf("%s-%s.capsule", m.ID, m.Version)
		}
		if err := capsule.Pack(dir, *out); err != nil {
			return err
		}
		fmt.Println("packed", *out)
		return nil
	case "verify":
		if len(args) != 2 {
			return errors.New("usage: luna capsule verify <folder|file.capsule>")
		}
		tmp, err := os.MkdirTemp("", "luna-verify-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		u, err := capsule.AddToStore(tmp, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("ok: %s %s, package %s\n", u.Manifest.ID, u.Manifest.Version, u.Hash[:12])
		return nil
	case "init", "dev", "search", "update", "runtime":
		return fmt.Errorf("luna capsule %s comes with the SDK and plugin index (Phase 3)", args[0])
	}
	return call(p, request{Cmd: "capsule", Args: args})
}

func installCapsule(p paths, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "don't ask, if these exact permissions were approved before")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: luna capsule install [--yes] <folder|file.capsule>")
	}
	src := fs.Arg(0)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("%s: no such folder or .capsule file (installing by name needs the plugin index, which doesn't exist yet)", src)
	}
	u, err := capsule.AddToStore(p.capsuleStore(), src)
	if err != nil {
		return err
	}
	m := u.Manifest
	if err := capsule.CanRun(u); err != nil {
		return err
	}
	text, fp := approval(m)
	prev, err := callOut(p, request{Cmd: "capsule", Args: []string{"approved", m.ID}})
	if err != nil {
		return err
	}
	if prev != fp {
		if *yes {
			return errors.New("these permissions haven't been approved before; run without --yes to review them")
		}
		fmt.Printf("%s %s (%s) asks for:\n%s", m.ID, m.Version, m.Runtime, text)
		if prev != "" {
			fmt.Println("  (its permissions changed since you last approved it)")
		}
		fmt.Print("Install? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return errors.New("not installed")
		}
	}
	return call(p, request{Cmd: "capsule", Args: []string{"register", u.Hash, fp}})
}

// startCapsules creates the runtime and starts every enabled capsule.
func startCapsules(p paths, st *store.Store, publish func(event.Event)) (*capsule.Runtime, error) {
	run := p.runDir()
	if err := os.MkdirAll(run, 0o700); err != nil {
		return nil, err
	}
	if err := ownedDir(run); err != nil {
		return nil, err
	}
	rt := &capsule.Runtime{
		RunDir:  run,
		DataDir: p.capsuleData(),
		OnState: func(id, state string, f *capsule.Failure) {
			d := map[string]any{"id": id, "state": state}
			if f != nil {
				d["cause"] = f.Cause
			}
			if ev, err := event.New("bot.plugin."+state, "luna", false, d); err == nil {
				publish(ev)
			}
		},
		// shortcut: the plugin API on the host socket is Phase 3; connections are closed for now.
	}
	rows, err := st.Capsules()
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		u, err := capsule.FromStore(p.capsuleStore(), r.Hash)
		if err == nil {
			_, err = rt.Enable(u, r.Uses)
		}
		if err != nil {
			slog.Error("capsule not started", "capsule", r.ID, "err", err)
		}
	}
	return rt, nil
}

func capsuleCmd(p paths, args []string, rt *capsule.Runtime, st *store.Store) (string, error) {
	rows, err := st.Capsules()
	if err != nil {
		return "", err
	}
	if len(args) == 0 {
		return "", errors.New("missing subcommand")
	}
	if args[0] == "list" {
		if len(rows) == 0 {
			return "no capsules installed", nil
		}
		var b strings.Builder
		for _, r := range rows {
			state, last := "disabled", ""
			if r.Enabled {
				state, last = "not started", "see the daemon log"
			}
			if c := rt.Get(r.ID); c != nil {
				last = ""
				state = c.State()
				if h := c.History(); len(h) > 0 {
					last = "last failure: " + h[len(h)-1].Cause
				}
			}
			fmt.Fprintf(&b, "%-32s %-10s %-10s %s\n", r.ID, r.Version, state, last)
		}
		return strings.TrimSuffix(b.String(), "\n"), nil
	}
	if len(args) < 2 {
		return "", fmt.Errorf("usage: luna capsule %s <name>", args[0])
	}
	if args[0] == "register" {
		return registerCapsule(p, args[1:], rt, st, rows)
	}
	var row *store.CapsuleRow
	for i, r := range rows {
		if r.ID == args[1] || strings.HasSuffix(r.ID, "."+args[1]) {
			if row != nil {
				return "", fmt.Errorf("%q matches more than one capsule; use the full ID", args[1])
			}
			row = &rows[i]
		}
	}
	if args[0] == "approved" {
		if row == nil {
			return "", nil
		}
		return row.Approved, nil
	}
	if row == nil {
		return "", fmt.Errorf("no capsule %q installed (see luna capsule list)", args[1])
	}
	c := rt.Get(row.ID)
	switch args[0] {
	case "enable", "restart":
		if c != nil && args[0] == "restart" {
			c.Start()
		} else if c == nil {
			u, err := capsule.FromStore(p.capsuleStore(), row.Hash)
			if err != nil {
				return "", err
			}
			if c, err = rt.Enable(u, row.Uses); err != nil {
				return "", err
			}
		}
		row.Enabled = true
		if err := st.SaveCapsule(*row); err != nil {
			return "", err
		}
		return waitRunning(c)
	case "disable":
		rt.Disable(row.ID)
		row.Enabled = false
		return "disabled " + row.ID, st.SaveCapsule(*row)
	case "uninstall":
		rt.Disable(row.ID)
		if err := st.DeleteCapsule(row.ID); err != nil {
			return "", err
		}
		msg := "uninstalled " + row.ID
		if len(args) > 2 && args[2] == "--keep-data" {
			msg += "; kept its data"
		} else if err := os.RemoveAll(filepath.Join(p.capsuleData(), row.ID)); err != nil {
			return "", err
		}
		gcStore(p, st)
		return msg, nil
	case "logs":
		if c == nil {
			return "", errors.New(row.ID + " is disabled; it has no logs this session")
		}
		if len(args) > 2 && args[2] == "-f" {
			// shortcut: one request, one reply on the control socket; following needs streaming (same as events tail).
			return "", errors.New("logs -f isn't supported yet; run luna capsule logs again")
		}
		return strings.Join(c.Logs.Tail(200), "\n"), nil
	case "inspect":
		return inspect(p, row, c)
	}
	return "", fmt.Errorf("unknown subcommand %q", args[0])
}

func registerCapsule(p paths, args []string, rt *capsule.Runtime, st *store.Store, rows []store.CapsuleRow) (string, error) {
	if len(args) != 2 {
		return "", errors.New("usage: register <hash> <approval>")
	}
	u, err := capsule.FromStore(p.capsuleStore(), args[0])
	if err != nil {
		return "", err
	}
	if _, fp := approval(u.Manifest); fp != args[1] {
		return "", errors.New("approval doesn't match this package's permissions; install again")
	}
	m := u.Manifest
	c, err := rt.Enable(u, m.Links.Uses)
	if err != nil {
		return "", err
	}
	err = st.SaveCapsule(store.CapsuleRow{ID: m.ID, Version: m.Version, Hash: u.Hash, Enabled: true, Uses: m.Links.Uses, Approved: args[1]})
	if err != nil {
		return "", err
	}
	gcStore(p, st)
	return waitRunning(c)
}

// waitRunning waits for a capsule's first start to settle.
func waitRunning(c *capsule.Capsule) (string, error) {
	deadline := time.Now().Add(8 * time.Second) // inside the control socket's 10 s request deadline
	for time.Now().Before(deadline) {
		switch c.State() {
		case capsule.Running:
			return c.ID + " is running", nil
		case capsule.Restarting, capsule.Failed, capsule.Stopped:
			if h := c.History(); len(h) > 0 {
				f := h[len(h)-1]
				return "", fmt.Errorf("%s didn't start (%s); last log lines:\n%s", c.ID, f.Cause, strings.Join(tail(f.Logs, 10), "\n"))
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return c.ID + " is still " + c.State() + "; check luna capsule list", nil
}

func tail(s []string, n int) []string { return s[max(0, len(s)-n):] }

func inspect(p paths, row *store.CapsuleRow, c *capsule.Capsule) (string, error) {
	u, err := capsule.FromStore(p.capsuleStore(), row.Hash)
	if err != nil {
		return "", err
	}
	m := u.Manifest
	state := "disabled"
	if c != nil {
		state = c.State()
	}
	text, _ := approval(m)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s (%s), %s\npackage %s\n%s", m.ID, m.Version, m.Runtime, state, u.Hash[:16], text)
	for _, ch := range sandbox.Doctor() {
		if !ch.OK {
			fmt.Fprintf(&b, "  backend gap: %s: %s\n", ch.Name, ch.Detail)
		}
	}
	if c != nil {
		for _, f := range c.History() {
			fmt.Fprintf(&b, "failed %s: %s", f.Time.Format(time.DateTime), f.Cause)
			if f.Code != 0 {
				fmt.Fprintf(&b, " (exit %d)", f.Code)
			}
			if f.Signal != "" {
				fmt.Fprintf(&b, " (%s)", f.Signal)
			}
			b.WriteByte('\n')
		}
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// gcStore removes store entries no installed capsule uses. Entries younger than an hour
// are kept, since an install may be waiting at its approval prompt.
func gcStore(p paths, st *store.Store) {
	rows, err := st.Capsules()
	if err != nil {
		return
	}
	used := map[string]bool{}
	for _, r := range rows {
		used[r.Hash] = true
	}
	ents, _ := os.ReadDir(p.capsuleStore())
	for _, e := range ents {
		info, err := e.Info()
		if err != nil || used[e.Name()] || time.Since(info.ModTime()) < time.Hour {
			continue
		}
		os.RemoveAll(filepath.Join(p.capsuleStore(), e.Name()))
	}
}
