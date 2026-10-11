package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"luna/internal/capsule"
	"luna/internal/capsule/sandbox"
	"luna/internal/secrets"
	"luna/internal/twitch"
)

var stdin = bufio.NewReader(os.Stdin)

// ask asks a yes/no question; an empty answer (or no terminal) takes the default.
func ask(q string, def bool) bool {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	fmt.Printf("%s %s ", q, hint)
	line, err := stdin.ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	if err != nil || a == "" {
		fmt.Println()
		return def
	}
	return a == "y" || a == "yes"
}

var setupSteps = []struct {
	name, title string
	run         func(paths, bool) error
}{
	{"check", "Check this machine", setupCheck},
	{"twitch", "Twitch broadcaster account", setupTwitch},
	{"bot", "Bot account", setupBot},
	{"runtime", "Python runtime for plugins", setupRuntime},
	{"integrations", "Integrations (OBS, VRChat, HRCollect)", func(paths, bool) error {
		fmt.Println("  not available yet; they arrive with the launch plugins (Phase 5)")
		return nil
	}},
	{"service", "Run Luna as a systemd user service", setupService},
	{"core", "Core bundle: Essentials, Moderation, Events, Shoutouts", setupCore},
	{"smoke", "Smoke test", setupSmoke},
}

// setup runs every step, or just the one named: luna setup [step] [--no-service].
func setup(p paths, args []string) error {
	noService := false
	var only string
	for _, a := range args {
		switch {
		case a == "--no-service":
			noService = true
		case only == "" && !strings.HasPrefix(a, "-"):
			only = a
		default:
			return exitErr{exitUsage, "usage: luna setup [check|twitch|bot|runtime|service|core|smoke] [--no-service]"}
		}
	}
	found := false
	for i, s := range setupSteps {
		if only != "" && s.name != only {
			continue
		}
		found = true
		if only == "" && s.name == "service" && noService {
			continue
		}
		fmt.Printf("\n%d. %s\n", i+1, s.title)
		if err := s.run(p, only != ""); err != nil {
			fmt.Printf("  ✗ %v\n  (rerun this step with: luna setup %s)\n", err, s.name)
			if s.name == "check" {
				return errors.New("this machine can't run capsules safely; fix the above first")
			}
		}
	}
	if !found {
		return exitErr{exitUsage, "unknown setup step " + only}
	}
	if only == "" {
		fmt.Println("\nDone. Next: luna capsule init <name> to write a plugin, or luna status.")
	}
	return nil
}

func setupCheck(p paths, _ bool) error {
	fmt.Printf("  config %s\n  data   %s\n", p.config, p.data)
	ok := true
	for _, c := range sandbox.Doctor() {
		mark := "✓"
		if !c.OK {
			mark = "✗"
		}
		fmt.Printf("  %s %-18s %s\n", mark, c.Name, c.Detail)
		if c.Name == "user namespaces" && !c.OK {
			ok = false
		}
	}
	if !ok {
		return errors.New("user namespaces are unavailable")
	}
	return nil
}

func setupTwitch(p paths, explicit bool) error {
	if t, err := twitch.LoadToken(secrets.Open(p.data), "broadcaster"); err == nil {
		fmt.Printf("  logged in as %s\n", t.Login)
		if !explicit && !ask("  Log in again?", false) {
			return nil
		}
	}
	return auth(p, "login", []string{"broadcaster"})
}

func setupBot(p paths, explicit bool) error {
	if t, err := twitch.LoadToken(secrets.Open(p.data), "bot"); err == nil {
		fmt.Printf("  bot account: %s\n", t.Login)
		if !explicit && !ask("  Log in a different bot account?", false) {
			return nil
		}
	} else if !explicit && !ask("  Chat from a separate bot account? (otherwise Luna chats as you)", true) {
		return nil
	}
	return auth(p, "login", []string{"bot"})
}

func setupRuntime(p paths, explicit bool) error {
	if info, err := capsule.ResolveRuntime(p.runtimes(), "python-3.12"); err == nil {
		fmt.Printf("  python %s installed\n", info.Version)
		return nil
	}
	if !explicit && !ask("  Install it now? (about 75 MB to download, 130 MB on disk)", true) {
		return nil
	}
	_, err := capsule.AddRuntime(p.runtimes(), "python-3.12", os.Stdout)
	return err
}

const unitTemplate = `[Unit]
Description=Luna, a self-hosted Twitch bot
Documentation=luna --help

[Service]
ExecStart=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`

func setupService(p paths, explicit bool) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("no systemctl here; start Luna with `luna daemon` from your session instead")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if strings.HasPrefix(exe, os.TempDir()) {
		fmt.Printf("  note: %s is in a temporary folder; install luna somewhere permanent first\n", exe)
	}
	argv := []string{exe}
	if p.custom {
		argv = append(argv, "--config", p.confDir)
	}
	argv = append(argv, "daemon")
	unit, err := unitPath()
	if err != nil {
		return err
	}
	if !explicit && !ask("  Write "+unit+" and start Luna now?", true) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		return err
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = systemdQuote(a)
	}
	if err := os.WriteFile(unit, []byte(fmt.Sprintf(unitTemplate, strings.Join(quoted, " "))), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{{"--user", "daemon-reload"}, {"--user", "enable", "--now", "luna.service"}} {
		cmd := exec.Command("systemctl", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	fmt.Println("  ✓ luna.service enabled and started (luna start/stop/restart control it)")
	return nil
}

// systemdQuote quotes an ExecStart argument when it holds spaces or quotes.
func systemdQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// waitDaemon gives a just-started daemon five seconds to answer.
func waitDaemon(p paths) error {
	var err error
	for i := 0; i < 20; i++ {
		if _, err = callOut(p, request{Cmd: "status"}); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("the daemon isn't reachable; start it with `luna start` or `luna daemon`")
}

// setupCore installs the core bundle, showing its permissions for approval like any plugin.
func setupCore(p paths, _ bool) error {
	if err := waitDaemon(p); err != nil {
		return err
	}
	fmt.Println("  Thank-you messages are off until you pick them: luna config set plugins.core.events_on follow,sub,raid")
	return installCapsule(p, []string{"core"})
}

func setupSmoke(p paths, _ bool) error {
	if err := waitDaemon(p); err != nil {
		return err
	}
	out, err := callOut(p, request{Cmd: "sim", Args: []string{"follow", "--user", "luna_smoke_test"}})
	if err != nil {
		return err
	}
	fmt.Println("  ✓ daemon answered;", out)
	status, _ := callOut(p, request{Cmd: "status"})
	for l := range strings.Lines(status) {
		fmt.Print("  ", l)
	}
	fmt.Println()
	return nil
}
