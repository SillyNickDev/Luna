package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"luna/internal/config"
)

const usageText = `usage: luna [--config <dir>] <command>

  run                   start the daemon
  sim <event> [flags]   fire a simulated event (luna sim --list)
  reload                re-read config; an invalid file keeps the last good one
  auth login [broadcaster|bot]
  auth status | logout [broadcaster|bot]
                        Twitch logins; the bot account is optional
  perms show | set <user> <rank> | check <user> <capability>
                        ranks: viewer, moderator, luna-mod, head-mod
  chat say <text>       send a chat message through the output guard
  config validate       check luna.toml without applying it
`

type paths struct{ config, data, sock string }

func main() {
	fs := flag.NewFlagSet("luna", flag.ExitOnError)
	confDir := fs.String("config", "", "config directory")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usageText) }
	fs.Parse(os.Args[1:])
	args := fs.Args()

	p, err := resolvePaths(*confDir)
	if err == nil {
		switch {
		case len(args) == 0:
			fs.Usage()
			os.Exit(2)
		case args[0] == "run":
			err = runDaemon(p)
		case args[0] == "sim" && len(args) > 1 && args[1] == "--list":
			for _, s := range simList {
				fmt.Printf("  %-7s %-26s luna sim %s %s\n", s.name, s.typ, s.name, s.usage)
			}
		case args[0] == "sim", args[0] == "reload", args[0] == "perms":
			err = call(p, request{Cmd: args[0], Args: args[1:]})
		case args[0] == "auth" && len(args) >= 2:
			err = auth(p, args[1], args[2:])
		case args[0] == "chat" && len(args) > 2 && args[1] == "say":
			err = call(p, request{Cmd: "chat", Args: args[2:]})
		case args[0] == "config" && len(args) == 2 && args[1] == "validate":
			if _, err = config.Load(p.config); err == nil {
				fmt.Println("config ok:", p.config)
			}
		default:
			fs.Usage()
			os.Exit(2)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "luna:", err)
		os.Exit(1)
	}
}

// resolvePaths uses ~/.config/luna and ~/.local/share/luna (OS equivalents elsewhere).
// --config <dir> keeps everything, data included, under that dir so a second channel can't collide.
func resolvePaths(confDir string) (paths, error) {
	var dataDir string
	switch {
	case confDir != "":
		dataDir = filepath.Join(confDir, "data")
	case os.Getenv("XDG_DATA_HOME") != "":
		dataDir = filepath.Join(os.Getenv("XDG_DATA_HOME"), "luna")
	case runtime.GOOS == "windows":
		dataDir = filepath.Join(os.Getenv("LOCALAPPDATA"), "luna")
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return paths{}, err
		}
		dataDir = filepath.Join(home, ".local", "share", "luna")
	}
	if confDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return paths{}, err
		}
		confDir = filepath.Join(base, "luna")
	}
	return paths{
		config: filepath.Join(confDir, "luna.toml"),
		data:   dataDir,
		sock:   filepath.Join(dataDir, "luna.sock"),
	}, nil
}
