package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"luna/internal/capsule/sandbox"
	"luna/plugins"
)

const usageText = `usage: luna [--config <dir>] <command>

  setup [step]          take a fresh install to a running bot
  daemon                run the bot in the foreground (also: run)
  start | stop | restart
                        control the systemd user service
  status | version      what's running; versions of Luna, the plugin API, capsule spec
  tui                   the dashboard: status, capsules, events, timeline, chat (mouse works)
  logs [-f] [--capsule <name>]
  events tail [--type twitch.*] [--json] | chat tail
  sim <event> [flags]   fire a simulated event (luna sim --list)
  events sessions | replay <n> [--speed N]
                        list recorded streams, replay one as simulated events
  reload                re-read config; an invalid file keeps the last good one
  auth login [broadcaster|bot|spotify|deepl]
  auth status | logout [broadcaster|bot|spotify|deepl]
                        Twitch logins (the bot account is optional), Spotify and DeepL
  perms show | set <user> <rank> | check <user> <capability>
                        ranks: viewer, moderator, luna-mod, head-mod
  chat say <text>       send a chat message through the output guard
  integrations list | test osc
  osc send <address> <value> | params
                        VRChat: poke a parameter by hand; the current avatar's parameters
  config show [key] | get <key> | set <key> <value> | edit | validate
  capsule install [--yes] <folder|file.capsule|name>
                        built in: core (Essentials, Moderation, Events, Shoutouts),
                        points, giveaways, stream, avatar, hrc, games, polls, queue,
                        songs, access
  mod timeline | undo <id>
                        moderation actions, with undo (also: luna core --help)
  capsule list | enable | disable | restart | logs | inspect <name>
  capsule uninstall <name> [--keep-data]
  capsule doctor | pack [dir] [-o file] | verify <path>
  capsule init <name> | dev [dir] [--live] | runtime add python-3.12
                        plugins, each in its own sandbox
  <plugin> <command> [args] [--json]
                        a plugin's own commands (luna <plugin> --help)
`

type paths struct {
	config, data, sock string
	confDir            string
	custom             bool // --config was given
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == sandbox.InitArg {
		sandbox.Init() // luna re-executes itself as each capsule's setup stage
	}
	if len(os.Args) > 1 {
		if p, ok := plugins.ByEntry(os.Args[1]); ok {
			p.Main() // a first-party plugin's capsule runs a copy of this binary
			return
		}
	}
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
		case args[0] == "events" && len(args) > 1 && args[1] == "tail":
			err = eventsTail(p, args[2:], false)
		case args[0] == "chat" && len(args) > 1 && args[1] == "tail":
			err = eventsTail(p, args[2:], true)
		case args[0] == "run", args[0] == "daemon":
			err = runDaemon(p)
		case args[0] == "version":
			fmt.Println(versionText())
		case args[0] == "tui":
			err = runTUI(p)
		case args[0] == "status":
			err = call(p, request{Cmd: "status"})
		case args[0] == "start", args[0] == "stop", args[0] == "restart":
			err = serviceCLI(args[0])
		case args[0] == "setup":
			err = setup(p, args[1:])
		case args[0] == "logs":
			err = logsCLI(p, args[1:])
		case args[0] == "config":
			err = configCLI(p, args[1:])
		case args[0] == "sim" && len(args) > 1 && args[1] == "--list":
			for _, s := range simList {
				fmt.Printf("  %-7s %-26s luna sim %s %s\n", s.name, s.typ, s.name, s.usage)
			}
		case args[0] == "sim", args[0] == "reload", args[0] == "perms", args[0] == "events", args[0] == "osc",
			args[0] == "integrations":
			err = call(p, request{Cmd: args[0], Args: args[1:]})
		case args[0] == "auth" && len(args) >= 2:
			err = auth(p, args[1], args[2:])
		case args[0] == "chat" && len(args) > 2 && args[1] == "say":
			err = call(p, request{Cmd: "chat", Args: args[2:]})
		case args[0] == "capsule":
			err = capsuleCLI(p, args[1:])
		case args[0] == "mod" && len(args) > 1: // Moderation's commands live in the core bundle
			err = call(p, request{Cmd: "plugin", Args: append([]string{"luna.core"}, args[1:]...)})
		case args[0] == "plugin" && len(args) > 1:
			err = call(p, request{Cmd: "plugin", Args: args[1:]})
		case !strings.HasPrefix(args[0], "-"):
			err = call(p, request{Cmd: "plugin", Args: args}) // a plugin's command group: luna hrc status
		default:
			fs.Usage()
			os.Exit(2)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "luna:", err)
		var ee exitErr
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		os.Exit(1)
	}
}

// resolvePaths uses ~/.config/luna and ~/.local/share/luna (OS equivalents elsewhere).
// --config <dir> keeps everything, data included, under that dir so a second channel can't collide.
func resolvePaths(confDir string) (paths, error) {
	custom := confDir != ""
	if custom {
		if abs, err := filepath.Abs(confDir); err == nil {
			confDir = abs
		}
	}
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
		custom:  custom,
		confDir: confDir,
		config:  filepath.Join(confDir, "luna.toml"),
		data:    dataDir,
		sock:    filepath.Join(dataDir, "luna.sock"),
	}, nil
}
