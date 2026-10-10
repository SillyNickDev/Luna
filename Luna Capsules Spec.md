# Luna Capsules Spec v0.1 (draft)

A Luna Capsule is one untrusted program run as an ordinary OS process inside the host's native isolation, with declared limits, no image layers, no daemon and no VM. The spec is independent of Luna, the Twitch bot: Luna is its first host, but any app needing safe plugins could embed a conforming runtime.

## Scope and terms

**In scope**: package format, runtimes, lifecycle, isolation guarantees, per-OS backends, the control channel, links between capsules, supervision, the `luna capsule` install and management commands, and conformance.

**Out of scope**: what plugins say to the host. The bot's plugin API rides on top of the capsule's data socket and is specified separately.

| Term | Meaning |
| --- | --- |
| Host | The application embedding the runtime (the bot) |
| Runtime | The library `that implements this spec, built into Luna and driven by its luna capsule subcommands` |
| Capsule | One running instance of a package, with its own ID, limits and sockets |
| Package | The files on disk: manifest, entry and assets |
| Backend | The OS-specific isolation implementation (Linux, Windows, macOS, wasm) |
| Shared runtime | A language interpreter installed once and mounted read-only into many capsules |
| Link | A brokered, typed channel between two capsules |

Requirement words follow RFC 2119: **MUST** is required for conformance, **SHOULD** is expected unless a backend documents why not, **MAY** is optional.

## Package format

A package is a folder, or the same folder zipped with a `.capsule` extension. The runtime MUST unpack archives to a content-addressed store before running them and MUST refuse a package whose hashes don't match.

```
hr-alerts/
├── capsule.toml        # capsule section (required)
├── plugin.toml         # host-specific section, ignored by the runtime
├── capsule.lock        # SHA-256 of every file (generated)
├── capsule.sig         # optional minisign/ed25519 signature of capsule.lock
├── main.py             # entry
└── assets/
```

The bot may merge both manifests into one `plugin.toml` with a `[capsule]` table; the runtime reads only that table.

### `capsule.toml` schema

| Key | Type | Required | Notes |
| --- | --- | --- | --- |
| `spec` | string | yes | Spec version, `"0.1"` |
| `id` | string | yes | Reverse-DNS, `[a-z0-9.-]`, ≤ 128 chars |
| `version` | semver | yes |  |
| `runtime` | string | yes | `native`, `wasm`, or a shared runtime like `python-3.12` |
| `entry` | string | yes | Path inside the package, or binary name for `native` |
| `args` | string\[\] | no | Extra arguments |
| `env` | table | no | Plain values only; host secrets are never passed in env |
| `limits.memory` | size | no | Default `64m`, hard cap enforced by the backend |
| `limits.cpu` | float | no | Share of one core, default `0.25` |
| `limits.pids` | int | no | Max processes/threads, default `32` |
| `limits.files` | int | no | Max open file handles, default `256` |
| `fs.writable` | string | no | `none` or `data` (a private `data/` dir that persists across restarts), default `data` |
| `fs.data_quota` | size | no | Default `32m` |
| `net` | string or string\[\] | no | `none` (default), or a list of `host:port` allowlist entries |
| `devices` | string\[\] | no | Named device classes (`bluetooth`, `serial`, `gpu`, `camera`); each needs user approval |
| `links.provides` / `links.uses` | string\[\] | no | See Control channel and links |
| `health.interval` / `health.timeout` | duration | no | Default `5s` / `2s` |
| `restart` | string | no | `always` (default), `on-failure`, `never` |

### Native binaries

A `native` package MAY ship per-platform entries as `bin/<os>-<arch>/<entry>` (for example `bin/linux-amd64/hr-alerts`). The runtime picks the matching one and fails with `no_platform_build` if none exists.

## Runtimes

Shared runtimes are where the memory saving comes from: there is one copy of each interpreter on disk, mapped read-only into every capsule, so the OS shares its code pages between them.

- **Layout**: `runtimes/<name>-<version>/` with a `runtime.toml` naming the interpreter binary, its library paths and a SHA-256 for the whole tree.
- **Resolution**: `python-3.12` matches the newest installed `3.12.x`. A missing runtime fails with `runtime_missing`; the host decides whether to fetch it.
- **Read-only**: runtimes MUST be mounted or exposed read-only. A capsule's packages go in its own folder (`vendor/` for Python, `node_modules/` for Node), never into the shared runtime.
- **No ambient packages**: the runtime MUST NOT expose the host's system Python, Node or library paths. Only the shared runtime and the capsule's folder are visible.

| Runtime | Entry | Notes |
| --- | --- | --- |
| `native` | A compiled binary | Smallest footprint; Go, Rust, C, Zig |
| `wasm` | A `.wasm` module (WASI preview 1) | Run by an embedded wazero instance inside a minimal native capsule; identical sandbox on every OS |
| `python-3.12` | A `.py` file | Embeddable CPython on Windows, standalone builds on Linux and macOS |
| `node-22` | A `.js` or `.mjs` file |  |

The `wasm` runtime still runs inside a native capsule, so a WASM escape lands in the OS sandbox rather than the host.

## Lifecycle

Every state change is reported to the host as a `bot.plugin.<state>` event (the host chooses the event name).

&#91;embedded content: capsule lifecycle · 8 states\]

A `failed` capsule stays down until the user or host restarts it, which resets the failure count.

## Isolation guarantees and threat model

The threat is a malicious or buggy plugin that a streamer installed from someone else. It must not be able to steal Twitch tokens, read the user's files, reach the network unasked, starve the machine, or talk to another plugin outside a link.

Every backend MUST guarantee:

1. **Filesystem**: read access only to the package folder and its shared runtime; write access only to its `data/` folder within quota. No access to the host's home, config or token store.
2. **Network**: no sockets beyond its control and link sockets when `net = "none"`. With an allowlist, outbound only to listed `host:port`; the runtime SHOULD enforce this through a local proxy where the OS can't filter by host.
3. **Processes**: cannot see, signal or debug any process outside its own tree. Its whole tree dies when the capsule stops.
4. **Resources**: memory, CPU, process count and open files are capped; exceeding the memory cap kills the capsule rather than swapping the host.
5. **Privilege**: runs as an unprivileged identity with no way to gain privileges (no setuid, no admin token, no new capabilities).
6. **IPC**: the only channels out are the sockets the runtime handed it. No shared memory, D-Bus, Windows named objects or clipboard.
7. **Devices**: none, unless a named class in `devices` was approved by the user for this package version.

**Not in scope**: kernel exploits, hardware side channels (Spectre-class) and a compromised host. The spec defends a normal desktop from plugins, not a multi-tenant server from hostile tenants.

Where a backend can't meet a guarantee, it MUST report it in luna `capsule doctor` and the host MUST show the user before running untrusted capsules there.

## Platform backends

Linux gets the strongest backend and ships first; Windows is close; macOS has known gaps, listed at the end of this section.

### Linux

- **Namespaces**: start the entry with `clone3` in new user, PID, mount, network, IPC and UTS namespaces. No root needed where unprivileged user namespaces are enabled.
- **Filesystem**: `pivot_root` into an empty tmpfs; bind-mount the package and shared runtime read-only, `data/` read-write, and a minimal `/dev` (`null`, `zero`, `urandom`). Fresh `/proc` for the new PID namespace.
- **Landlock** (kernel 5.13+) as a second filesystem layer, so a mount-namespace escape still can't read outside allowed paths.
- **seccomp-bpf**: default-allow with a deny list (`ptrace`, `mount`, `bpf`, `perf_event_open`, `keyctl`, `kexec_*`, `init_module`, new user namespaces), plus `PR_SET_NO_NEW_PRIVS`.
- **Limits**: a cgroup v2 child per capsule when the user's session has a delegated cgroup (systemd user sessions usually do); otherwise `setrlimit` plus the supervisor's RSS watchdog.
- **Network**: the namespace has only loopback. An allowlist is served by the runtime's egress proxy, bridged onto the capsule's loopback so standard `HTTP_PROXY` clients work.
- **Caveat**: some distros restrict unprivileged user namespaces through AppArmor (recent Ubuntu). The runtime then needs a shipped AppArmor profile, and luna `capsule doctor` MUST say so.

### Windows (10 1809+)

- **AppContainer**: each package gets its own AppContainer profile and SID, launched with no capabilities. This blocks the user's files, the registry outside its hive, and the network by default.
- **Filesystem**: the runtime grants that SID read ACLs on the package and shared runtime and read-write on `data/`.
- **Job Object**: memory cap, CPU rate control, active process limit, and kill-on-close so the whole tree dies with the capsule.
- **Mitigations**: SHOULD apply child-process and extension-point policies; MAY skip dynamic-code blocking for JIT runtimes (Node).
- **Network**: allowlisted capsules get the `internetClient` capability plus Windows Filtering Platform rules keyed on their SID, permitting only listed hosts (resolved by the runtime) and ports.
- **Sockets**: AF\_UNIX socket files, with ACLs for the capsule's SID only.

### macOS (13+)

- **Sandbox profile**: launch through the Seatbelt sandbox with a generated deny-by-default profile allowing reads of the package and runtime, writes to `data/`, and only the capsule's socket paths. The API is deprecated but still used by major browsers; the spec should track any replacement.
- **Signals and processes**: the profile denies signals and process info outside the capsule's own tree.
- **Limits**: `setrlimit` for files and processes; memory is enforced by the supervisor's watchdog because macOS doesn't enforce an address-space cap reliably.
- **Network**: denied by the profile; allowlists go through the egress proxy on the capsule's Unix socket.

### Known gaps

| Guarantee | Linux | Windows | macOS |
| --- | --- | --- | --- |
| Memory cap | Hard (cgroup) or watchdog | Hard (Job Object) | Watchdog only |
| Hide other processes | Yes (PID ns) | Partly | Denied, not hidden |
| Network allowlist by host | Proxy | WFP rules | Proxy |
| Runs without admin/root | Yes (most distros) | Yes | Yes |

## Control channel and links

Each capsule gets three kinds of socket, each dedicated to one job, and no other way to talk to anything.

| Socket | Env var | Peer | Protocol |
| --- | --- | --- | --- |
| Control | `CAPSULE_CONTROL` | Runtime | This spec (below) |
| Host | `CAPSULE_HOST` | Host app | Defined by the host (the bot's plugin API; mounted as `/run/bot/plugin.sock`) |
| Link | `CAPSULE_LINKS` (a folder) | Runtime broker, never the other capsule | This spec (below) |

All sockets are Unix domain sockets (AF\_UNIX on Windows) created by the runtime, readable only by that capsule's identity.

### Framing

Control and link messages share one frame: a 4-byte big-endian length, then a CBOR map `{t: type, id: uint?, b: body}`. Frames over 1 MiB MUST be rejected and the connection closed.

### Control messages

| Type | Direction | Body | Meaning |
| --- | --- | --- | --- |
| `hello` | capsule → runtime | `spec`, `pid` | First frame, within 10 s of start |
| `welcome` | runtime → capsule | `id`, `limits`, `links` | Accepts the capsule |
| `ready` | capsule → runtime | none | Moves state to `running` |
| `ping` / `pong` | runtime ↔ capsule | none | Health check |
| `link_up` / `link_down` | runtime → capsule | `name` | A link became available or dropped |
| `shutdown` | runtime → capsule | `deadline_ms` | Finish and exit before the deadline |
| `log` | capsule → runtime | `level`, `msg` | Structured log line; stdout and stderr are also captured |

### Links

A link is a named, one-way-initiated request channel. Provider `A` declares `provides = ["overlay:show"]`; consumer `B` declares `uses = ["dev.sable.overlay:show"]`.

1. The runtime opens a link only when both declarations match, the user approved them at install, and the host's policy allows it.
2. `B` connects to `$CAPSULE_LINKS/dev.sable.overlay:show`. That socket is owned by the runtime's broker; `B` never gets a path into `A`.
3. The broker forwards `call {id, method, body}` frames to `A` and `result {id, ok, body}` frames back, stamping each call with `B`'s capsule ID so `A` always knows who's asking and it can't be spoofed.
4. The broker enforces a size cap (default 64 KiB per message), a rate limit (default 50 calls/s per link) and a 5 s call timeout, and logs every call's metadata.
5. If `A` is restarting or failed, calls return `link_unavailable` immediately, and `B` gets `link_down` and later `link_up`.

Links are request/response only in v0.1. Streams and broadcast are an open question.

## Supervision and restarts

A failing capsule is restarted alone, with backoff, and given up on after five failures in ten minutes; nothing else on the host restarts.

**Failure causes** are recorded on every exit, so the host can show why a plugin died:

| Cause | Detected by |
| --- | --- |
| `exit` | Process exited with a non-zero code (code recorded) |
| `crash` | Killed by a signal or unhandled exception |
| `oom` | Hit the memory cap |
| `unhealthy` | Two missed `pong`s in a row |
| `handshake` | No `hello` within 10 s, or a bad spec version |
| `policy` | Tried something the backend blocked hard enough to kill it (seccomp kill, Job Object violation) |

**Restart policy**

- `always` restarts on any exit, `on-failure` skips clean exits (code 0), `never` stops.
- Backoff doubles from 1 s to a 60 s cap; it resets after the capsule has run for 10 minutes without failing.
- Five failures inside a 10-minute window moves it to `failed`. Only the user or host can restart a failed capsule.
- Restarting reuses the same `data/` folder, so state written there survives. Memory state doesn't.

**Shutdown**: send `shutdown`, wait for the deadline (default 5 s), then kill the whole process tree. The runtime MUST NOT leave orphaned processes.

**Logs**: the runtime keeps a ring buffer per capsule (default 1 MiB) of `log` frames, stdout and stderr, and writes the last 200 lines to the failure record when a capsule dies.

## Tooling and conformance

There is no separate capsule tool: the runtime is a library inside Luna, and everything is a `luna capsule` subcommand. Another host embedding the spec would ship its own front end over the same library.

### Installing

`luna capsule install hrc` is the normal path once a plugin is published:

1. **Resolve** the name: a short name (`hrc`) looks up the configured plugin index; a full ID (`dev.sable.hrc`), a local folder, a `.capsule` file or an HTTPS URL is used directly. `hrc@0.3` pins a version.
2. **Fetch and verify**: download, check every hash in `capsule.lock`, and check the signature if the index requires one. Any mismatch aborts.
3. **Runtime**: if the named shared runtime (`python-3.12`) is missing, offer to install it.
4. **Approve**: show the permissions, network allowlist, devices and links in plain words, and wait for a yes. `--yes` skips this only for packages already approved at the same version and permission set.
5. **Register and start**: unpack to the content-addressed store, add it to Luna's config, start the capsule, and wait for `running`.

Updates repeat steps 2 to 5 and only ask again if the permissions grew.

### Commands

| Command | Does |
| --- | --- |
| `luna capsule install <name>` | Resolve, verify, approve, register and start a plugin |
| `luna capsule update [<name>]` | Update one or all; re-prompts only if permissions grew |
| `luna capsule uninstall <name> [--keep-data]` | Stop, unregister and remove it (and its `data/` unless kept) |
| `luna capsule list` | Installed capsules with version, state and last failure cause |
| `luna capsule enable` / `disable <name>` | Start or stop without uninstalling |
| `luna capsule restart <name>` | Manual restart; clears a `failed` state |
| `luna capsule logs <name> [-f]` | Show or follow the log ring buffer |
| `luna capsule inspect <name>` | Limits, granted permissions, links, backend and failure history |
| `luna capsule search <query>` | Search the plugin index |
| `luna capsule doctor` | Report which guarantees this machine's backend can meet |
| `luna capsule runtime add python-3.12` | Install a shared runtime |
| **For plugin authors** |  |
| `luna capsule init --runtime python-3.12` | Scaffold a package |
| `luna capsule dev ./pkg` | Run a local folder against the event simulator, restarting on file changes |
| `luna capsule pack` / `verify` | Build `capsule.lock`, zip and sign; check hashes and signature |

### Conformance suite

A backend conforms when it passes the suite of hostile test capsules, each trying to break one guarantee and expected to fail:

- Read `~/.ssh`, the host's token store and another capsule's `data/`.
- Open a TCP connection with `net = "none"`, and reach a non-allowlisted host with an allowlist.
- Allocate past the memory cap; fork-bomb past `limits.pids`; open files past `limits.files`.
- Signal or inspect a process outside its tree; leave a child running after shutdown.
- Connect to another capsule's link socket directly, spoof a caller ID, send a 2 MiB frame.

Results publish as a per-OS table in the repo so plugin authors and users can see each backend's real strength.

## Open questions

- [ ] Link streams and broadcast (needed for high-rate data like heart rate or tracking).
- [ ] Whether the egress proxy should also do TLS pinning per allowed host.
- [ ] macOS: track Seatbelt deprecation; consider a dedicated user account per capsule to harden process isolation.
- [ ] Package signing: who signs (author keys, a registry, or both) and how revocation works.
- [ ] Ship the spec and runtime as a standalone repo, separate from the bot.
