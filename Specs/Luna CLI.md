# Luna CLI

One `luna` binary is both the bot and its remote control: `luna daemon` runs the bot, and every other command is a thin client that talks to the running daemon over a local control socket.

## How it works

- **Daemon and client**: commands that change live state (`capsule install`, `config set`, `sim`) send a request to the daemon and print its answer. If no daemon is running, read-only commands (`config show`, `capsule list`) read the files directly, and the rest say how to start it.
- **Control socket**: `$XDG_RUNTIME_DIR/luna/control.sock` on Linux, a named pipe on Windows, readable only by the user who started Luna. Nothing listens on the network.
- **Remote use**: `luna --host nick@homelab <cmd>` runs the same command over SSH against a daemon on another machine, so a homelab install is managed from the laptop without opening a port.
- **Grammar**: `luna <group> <verb> [args] [flags]`, nouns before verbs (`luna capsule install`, not `luna install-capsule`). Top-level shortcuts exist only for the commands used most (`luna status`, `luna logs`).
- **Plugins extend it**: an installed capsule can add its own command group (`luna hrc status`). See Plugin commands.
- **Same commands from chat**: the broadcaster, head mods and Luna mods can run allowlisted CLI commands from Twitch chat as `!luna <cmd>`, with a per-command allowlist, so things like `!luna capsule restart hrc` work mid-stream.

## Command map

&#91;embedded content: luna command groups\]

Plugin groups appear in `luna --help` under their own heading, so it's always clear which commands are built in and which came from a capsule.

## Setup

`luna setup` takes a fresh install to a running bot in one interactive pass. Every step can be skipped and rerun alone with `luna setup <step>`.

1. **Check** (`luna setup check`): OS, capsule backend strength (runs `luna capsule doctor`), and whether ports or devices it may need are free.
2. **Paths**: config dir (`~/.config/luna`), data dir (`~/.local/share/luna`), and whether to run as a user service.
3. **Twitch broadcaster** (`luna setup twitch`): device code login for your channel account. Shows the code and URL, waits for approval, lists the scopes it got.
4. **Bot account**: log in a second account for chat, or reuse the broadcaster account.
5. **Integrations**: optional, one at a time. **OBS** asks for the obs-websocket address and password and tests the connection. **VRChat** asks for the OSC send/receive ports (default 9000/9001) and sends a test chatbox message. **HRCollect** asks for the server URL and shows a live BPM reading to confirm.
6. **Starter capsules**: tick-list of first-party plugins (commands, timers, moderation, shoutouts, counters) installed through the normal `luna capsule install` flow.
7. **Service**: on Linux, writes a systemd user unit and enables it; on Windows, registers a per-user scheduled task at logon. `--no-service` skips this.
8. **Smoke test**: starts the daemon, fires a simulated follow, and confirms the bot replied in chat (or prints why it couldn't).

Non-interactive installs use `luna setup --from setup.toml` with the same answers in a file, which suits a homelab or a reinstall.

## Running and managing Luna

### Daemon

| Command | Does |
| --- | --- |
| `luna daemon` | Run the bot in the foreground (what the service calls) |
| `luna start` / `stop` / `restart` | Control the installed service |
| `luna status` | One screen: daemon uptime, Twitch connection, each integration's state, capsules running/failed, chat rate-limit headroom |
| `luna reload` | Re-read config and restart only what changed; the Twitch connection stays up |
| `luna update` | Update Luna itself (checks the signature), then restart |
| `luna version` | Versions of Luna, the plugin API and the capsule spec |

### Auth

| Command | Does |
| --- | --- |
| `luna auth status` | Accounts, scopes and token expiry |
| `luna auth login [broadcaster\|bot]` | Device code login for one account |
| `luna auth scopes add <scope>` | Request an extra scope (a capsule install does this automatically when it needs one) |
| `luna auth logout [account]` | Revoke and delete tokens |

### Config

Config is plain TOML on disk; these commands edit it safely and validate before applying.

| Command | Does |
| --- | --- |
| `luna config show [<key>]` | Print effective config, secrets masked |
| `luna config get` / `set <key> <value>` | Read or change one value, e.g. `luna config set chat.rate_limit 18` |
| `luna config edit` | Open in `$EDITOR`; validate on save, keep the last good file if invalid |
| `luna config validate` | Check without applying |
| `luna integrations list` | OBS, OSC, HRCollect and their connection state |
| `luna integrations test <name>` | Re-run that integration's setup test |

### Backup

| Command | Does |
| --- | --- |
| `luna backup [file]` | One archive with config, the SQLite database and capsule `data/` folders; tokens excluded unless `--with-secrets` |
| `luna restore <file>` | Restore it; asks before overwriting |
| `luna export viewers [--format csv]` | Your viewer data, since it's yours |

## Stream tools

| Command | Does |
| --- | --- |
| `luna events tail [--type twitch.*]` | Live stream of bus events, filterable by type or source |
| `luna events replay <session> [--speed 4]` | Replay a recorded stream's events into the bus (marked `simulated`) |
| `luna events sessions` | List recorded sessions by date |
| `luna sim <event> [flags]` | Fire one fake event: `luna sim sub --user test --tier 2`, `luna sim raid --viewers 40`, `luna sim redeem "Toggle ears"`, `luna sim hr 150` |
| `luna sim --list` | Every event type and its flags |
| `luna chat say <text>` | Send a message as the bot |
| `luna chat tail` | Watch chat in the terminal |
| `luna logs [-f] [--capsule hrc]` | Luna's own log, or one capsule's |
| `luna mod timeline [--since 1h]` | Timeouts, bans, deletions and bot actions, newest first |
| `luna mod undo <action-id>` | Reverse one action (unban, untimeout, restore a counter) |
| `luna osc send <address> <value>` | Poke an avatar parameter by hand, for testing mappings |
| `luna obs scene <name>` | Switch OBS scene |

Simulated events reach capsules exactly like real ones, but chat output from them goes to the terminal instead of Twitch unless `--live` is passed.

## Plugin commands

A capsule declares its commands in its manifest and handles them in its own code. Luna owns parsing, help, completion, permissions and cooldowns, so every plugin's commands look and behave like built-ins.

### Namespace

- Each capsule with CLI commands gets one group: `luna <alias> <command>`. The alias comes from `cli.name` in the manifest (`hrc`) and can be changed at install with `luna capsule install hrc --as heart`.
- Built-in group names (`setup`, `capsule`, `config`, `auth`, `sim`, `events`, `mod`, `chat`, `logs` and the rest) are reserved. A clash makes the install ask for an alias.
- The fully qualified form always works: `luna plugin dev.sable.hrc status`.

### Declaring commands

```toml
[cli]
name = "hrc"

[[cli.commands]]
name = "status"
summary = "Show the current heart rate and source device"

[[cli.commands]]
name = "threshold"
summary = "Get or set the alert threshold"
args  = [{ name = "bpm", type = "int", optional = true }]
flags = [{ name = "reset", type = "bool" }]
mutates = true

[[chat.commands]]
name = "hr"
aliases = ["heartrate"]
roles = ["everyone"]
cooldown = { global = "10s", user = "30s" }
```

Because commands live in the manifest, `luna hrc --help` and shell completion work even while the capsule is stopped.

### Handling them

```python
@bot.cli("threshold")
def threshold(bpm: int | None = None, reset: bool = False):
    if reset:
        bot.store.delete("threshold")
    elif bpm is not None:
        bot.store.set("threshold", bpm)
    return {"threshold": bot.store.get("threshold") or bot.config["threshold"]}

@bot.chat_command("hr")
def hr(ctx):
    ctx.reply(f"{bot.last_bpm} bpm")
```

### How a call flows

1. The CLI sends the command to the daemon, which checks args and flags against the manifest schema. Bad input never reaches the capsule.
2. The daemon sends `cli.invoke {command, args, flags, interactive}` over the capsule's host socket.
3. The capsule returns structured data, and optionally text. Luna renders data as a table in a terminal or JSON with `--json`, so plugins never format output.
4. Calls time out after 30 s by default; Ctrl-C sends `cli.cancel`. A capsule that isn't running returns exit code 4 with its last failure cause.
5. A capsule MAY ask `cli.prompt` for a confirmation or text, but only when the call is interactive; otherwise the call fails with exit code 2.

### Permissions

- Running a plugin's command never grants it extra permissions; it acts only within its manifest.
- `mutates = true` commands are allowed from the CLI for the Luna owner, and from chat for Luna mods and above when listed in `chat.cli_allowlist`.
- Chat commands get capability checks (Permissions tab), cooldowns and spam checks applied by Luna before the capsule sees them. Two capsules claiming the same chat command is refused at install unless one is aliased.

## Global flags, output and exit codes

| Flag | Does |
| --- | --- |
| `--json` | Machine-readable output (one JSON document, or JSON lines for `tail`/`-f`) |
| `--host <ssh-target>` | Run against a daemon on another machine over SSH |
| `--config <dir>` | Use another config directory (a second channel, or testing) |
| `-y`, `--yes` | Accept prompts that don't widen permissions |
| `-q`, `--quiet` / `-v`, `--verbose` | Less or more output |
| `--no-color` | Plain text; also when `NO_COLOR` is set or output isn't a terminal |

| Exit code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Command failed (the error says why) |
| 2 | Bad usage or input, or a prompt needed in non-interactive mode |
| 3 | Daemon not running or not reachable |
| 4 | Target capsule not running or failed |
| 5 | Permission denied or approval declined |
| 6 | Twitch auth missing or expired |

Shell completions for bash, zsh, fish and PowerShell come from `luna completion <shell>` and include plugin commands; installs and uninstalls refresh them.
