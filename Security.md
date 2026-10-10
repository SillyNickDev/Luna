# Security

Luna treats chat, plugins and the network as hostile by default. Its job is to remove the building blocks attackers chain together: memory corruption, injection, path tricks, SSRF and token theft. That way no single bug turns into control of the stream or the machine.

## Threat model

**What's protected**, most valuable first:

1. Twitch tokens (broadcaster tokens can ban, run ads, change the channel).
2. The streamer's machine and files.
3. The stream itself: what appears in chat, on screen and on the avatar.
4. Viewer data Luna stores.

| Attacker | Can do | Main defences |
| --- | --- | --- |
| Viewer in chat | Send any text, redeem rewards, raid with bots | Treat all chat as data; strict parsing; per-user limits; output guard |
| Malicious or buggy plugin | Run arbitrary code inside its capsule | Capsule isolation; permissions; brokered links; no secrets in capsules |
| Network attacker | Intercept or spoof traffic | TLS everywhere with verification; signed updates and packages |
| Compromised plugin index or dependency | Ship malicious code to many users | Signatures, hashes, permission diffs on update, reproducible builds |
| Other local program as the same user | Talk to Luna's sockets, read files | Peer checks on sockets; encrypted tokens; tight file permissions |

Out of scope: an attacker who already has root or admin, or who controls the streamer's Twitch account directly.

## Trust boundaries

&#91;embedded content: trust boundaries · 4 zones\]

Every arrow is a boundary where input is parsed against a fixed schema and size-limited before Luna acts on it. Capsules reach the internet only through the egress proxy, and reach each other only through the link broker.

## Exploit primitives

Each row is a common building block of real exploits and the design choice that takes it away, not just a check that tries to catch it.

| Primitive | How Luna removes it |
| --- | --- |
| **Memory corruption** (overflows, use-after-free) | Core in Go with no cgo. `unsafe` only in one audited package, enforced by a lint rule in CI. Native capsule crashes stay inside the capsule. |
| **Command injection** | Luna never runs a shell. Processes start through `exec` with argument arrays, and a lint rule bans `sh -c` and `cmd /c`. |
| **SQL injection** | Prepared statements only; a lint rule rejects string-built SQL. Plugins never see SQL, only the `store` API. |
| **Path traversal and zip-slip** | Packages unpack with every path checked to stay inside the target; absolute paths, `..`, symlinks and hard links in archives are rejected. On Linux, file opens use `openat2` with `RESOLVE_BENEATH`. |
| **Symlink and TOCTOU races** | Open by handle and act on the handle; `O_NOFOLLOW` for config and data files; atomic write by temp file plus rename. |
| **Unsafe deserialisation** | No formats that construct arbitrary types. JSON, CBOR, TOML and protobuf decode into fixed schemas with size and depth limits, and unknown fields are dropped. |
| **ReDoS** | All regexes (banned phrases, filters, plugin `when` patterns compiled in the core) use Go's RE2 engine, which runs in linear time. Pattern length is capped. |
| **SSRF** | Capsule HTTP goes through the egress proxy, which resolves DNS itself, pins the IP for the connection and blocks loopback, private, link-local and cloud-metadata ranges unless explicitly allowed. This also defeats DNS rebinding. |
| **Chat and command injection** | Outgoing chat is sent as plain text through Helix; text starting with `/` or `.` is escaped so viewer input relayed by a plugin can't become a command. Chat mentions are rate-limited to stop mass pings. |
| **Privilege escalation via Luna** | Refuses to run as root or admin. Capsules get no-new-privileges, dropped capabilities and restricted tokens (Capsule spec). |
| **Identity spoofing** | Roles come from Twitch's verified user ID and badges in EventSub payloads, never from display names or message text. Link calls carry a broker-stamped caller ID. |
| **Denial of service** | Every queue, buffer and log is bounded (Performance tab); per-user and per-plugin rate limits; frame and payload size caps; capsule CPU and memory caps. |
| **Unicode tricks** | Usernames and commands are compared after NFKC normalisation and case folding. Bidi override and zero-width characters are stripped from command names, and banned-phrase matching runs on a confusables-folded copy. |
| **Log injection** | Logs are structured; newlines and control characters in user-supplied fields are escaped, so a viewer can't forge log lines. |

## Secrets and Twitch auth

Tokens exist in exactly one place, the core, and nothing that runs third-party code can read them.

- **Storage**: the OS keyring (Secret Service on Linux, Credential Manager on Windows, Keychain on macOS). Without a keyring, tokens are encrypted with a key file readable only by the user (`0600` or an owner-only ACL), and `luna doctor` warns about it.
- **Never leaves the core**: capsules, logs, backups (unless `--with-secrets`), crash reports and `config show` never contain tokens. A redaction layer in the logger masks anything shaped like a token as a backstop.
- **Least scope**: each feature requests only the scopes it needs, added at install time with the user seeing why. Unused scopes can be dropped with `luna auth scopes prune`.
- **Split accounts**: chat goes through the bot account. The broadcaster token is used only for actions that require it (mod actions, redeems, EventSub subscriptions), which limits damage if the bot account is compromised.
- **Refresh and revoke**: tokens refresh automatically before expiry; `luna auth logout` revokes them with Twitch, not just locally. If a token is rejected unexpectedly, Luna stops using it and alerts rather than retrying in a loop.
- **Integration secrets** (OBS websocket password, HRCollect key) are stored the same way as Twitch tokens.
- **In memory**: secrets sit in dedicated byte buffers that are overwritten after use where the Go runtime allows. This is best-effort, since garbage-collected memory can't be fully scrubbed.

## Local surface

### Sockets

- **No network listeners by default.** The control socket, capsule sockets and optional metrics endpoint are local only. The metrics endpoint binds `127.0.0.1` and exposes counters, never content.
- **Peer checks**: every control-socket connection is checked with `SO_PEERCRED` on Linux, `getpeereid` on macOS, or the named pipe's client process token on Windows. A process running as another user is refused even if file permissions are wrong.
- The socket directory is created owner-only (`0700`), and Luna refuses to start if it's owned by someone else or world-writable.

### Commands from chat

`!luna <cmd>` is powerful, so it's locked down:

- Off by default; enabled per command through an allowlist in config.
- Accepted only from users whose rank holds the luna.chat\_cli capability (broadcaster, head mods and Luna mods by default; see Permissions), identified by user ID from the EventSub payload. Plain moderators and viewers can never use it.
- Commands that widen permissions (`capsule install`, `auth`, `config set` on security keys, `update`) can never run from chat.
- Every chat-issued command appears in the mod timeline and can be undone where possible.

### Files

- Config, database and data directories are created owner-only. Luna warns and refuses to load a config file other users can write.
- Config can't point outside Luna's directories for anything a capsule will read.
- The SQLite database never holds tokens; viewer data in it can be exported or deleted per viewer (`luna export viewers`, `luna viewers forget <user>`).

## Network, updates and supply chain

### Network

- TLS 1.2+ with full certificate verification on every outbound connection. There's no `insecure` flag in release builds.
- Local integrations (OBS, HRCollect) on another machine must use TLS or a Tailscale/WireGuard path; plain connections to non-loopback addresses need an explicit `allow_plaintext` with a warning.
- EventSub messages are de-duplicated by message ID, and messages older than 10 minutes are rejected to stop replays.

### Updates

- `luna update` downloads releases signed with an offline ed25519 key whose public half is compiled into Luna. A mismatched or missing signature aborts.
- **Rollback protection**: Luna refuses to "update" to a lower version unless `--allow-downgrade` is passed by hand.
- Updates are atomic: the new binary is verified, swapped in by rename, and the old one kept for one-step rollback.

### Plugins

- Every package is hash-locked; the index can require author signatures, and users can pin trusted author keys.
- An update that widens permissions, network allowlists, devices or links pauses for approval and shows exactly what changed.
- `luna capsule audit` lists every installed plugin's permissions, so the riskiest ones are easy to spot.

### Luna's own supply chain

- Minimal dependencies, each one justified in the repo. All are pinned via `go.sum` and checked with `govulncheck` in CI and before release.
- Reproducible builds: anyone can rebuild a release from source and get the same bytes. Releases ship checksums, signatures and an SBOM.
- CI uses pinned action versions, least-privilege tokens and no secrets in pull-request builds from forks.

## Verification

- **Fuzzing** with Go's built-in fuzzer, running continuously in CI, on everything that parses untrusted input: EventSub payloads, chat command parsing, the capsule frame decoder, manifest and config parsers, package unpacking, and the egress proxy's URL and host handling.
- **Hostile test suite**: the capsule conformance tests (Capsule spec) plus Luna-level ones. These include a plugin trying to read tokens, a viewer sending a 500-character zalgo command, a redeem input with `/ban`, and a package with a `../` path.
- **Static checks** in CI: `go vet`, `staticcheck`, `gosec`, the custom lint rules above (no `unsafe`, no shell, no string-built SQL), and `govulncheck`.
- **Security review** before every minor release, using a checklist of the primitives table; an external audit before 1.0, focused on the capsule backends.
- **Disclosure**: `SECURITY.md` with a private reporting channel (GitHub security advisories), a 90-day disclosure window, and credit for reporters. Fixes ship as patch releases, and plugins can be revoked from the index.

### Secure defaults at a glance

- [ ] No network listeners
- [ ] Capsules have no network, no devices and no files outside their folder
- [ ] Chat-issued Luna commands off
- [ ] Plaintext connections to remote hosts refused
- [ ] Unsigned updates refused
- [ ] Running as root or admin refused
