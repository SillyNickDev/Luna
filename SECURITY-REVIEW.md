# Luna security review pack

For an external auditor: what Luna protects, where every trust boundary is enforced, how
it's tested, and the limits we already know about. Read `Specs/Security.md` and
`Specs/Luna Capsules Spec.md` for the intent; this file maps the intent to code.

## What Luna is

One static Go binary (`CGO_ENABLED=0`) that a streamer runs on their own desktop (Linux;
Windows backend untested). It holds the streamer's Twitch, Spotify and DeepL credentials,
reads Twitch chat and events, talks to VRChat over OSC on loopback, and runs plugins
("capsules") from third parties, each in an OS sandbox. One user, one machine; not a
multi-tenant server.

## Assets, in order

1. **Credentials**: Twitch broadcaster and bot tokens (they can ban, edit the channel, raid,
   run predictions with viewers' channel points), Spotify token, DeepL key.
2. **The streamer's machine**: files, other processes, the LAN behind it.
3. **The stream**: what is said in chat, moderation actions, the avatar, the music.
4. **Viewer data**: watch time, points, notes, in `luna.db`.

## Adversaries

| Adversary | Can | Must not be able to |
| --- | --- | --- |
| Viewers in chat | Send arbitrary text, names, links | Run commands above their rank, inject chat commands into Luna's output, exhaust Luna |
| A hostile plugin (the main one) | Anything its sandbox allows, any bytes on its sockets | Read credentials or files, reach the network or LAN beyond its allowlist, act beyond its approved permissions, talk to other plugins outside links, starve the machine |
| The network | MITM, DNS tricks | Read or forge Twitch/Spotify/DeepL traffic, serve a tampered plugin |
| Other local users | Connect to sockets | Control the daemon |
| Local apps of the same user | Send OSC to 127.0.0.1:9001 | (accepted: they can already do anything the user can) |

## Trust boundaries

Each row: where untrusted input enters, what enforces the rule, and the tests. Paths are
from the repo root.

| # | Boundary | Enforcement | Tests |
| --- | --- | --- | --- |
| 1 | **Capsule sandbox, Linux**: plugin process ↔ host | `internal/capsule/sandbox/linux.go`, `init_linux.go`: user/PID/mount/net/IPC/UTS namespaces, `pivot_root` onto tmpfs, read-only binds, Landlock, seccomp deny list + `NO_NEW_PRIVS`, cgroup v2 caps via systemd (rlimits + RSS watchdog fallback) | `sandbox.TestIsolation`, `TestLimits`, `TestNoOrphans`, `TestFileMount`; matrix in `CONFORMANCE.md` |
| 2 | **Capsule sandbox, Windows**: same | `internal/capsule/sandbox/windows.go`: AppContainer with no capabilities, ACL grants per mount, Job Object (memory, CPU, processes, UI/clipboard limits, kill on close), child-process and extension-point mitigations | `sandbox/windows_test.go` (**written, not yet run on Windows**) |
| 3 | **Plugin API**: gRPC over the capsule's Unix socket | `internal/plugin/server.go`, `integrations.go`: every call checked against the approved manifest (`[permissions]`), size caps (args 4 KiB, store values 64 KiB, 10 000 keys), plugin store namespaced by plugin ID, simulated writes only printed, `Emit` limited to `plugin.<id>.*` | `TestHostAPI`, `TestNoPermissions`, `TestManifestRefusals`, `TestModTwitchViewers`, `TestUnbanNeedsBanForPermanentBans`, `TestOSCPermissions`, `FuzzOSCValue` |
| 4 | **Control channel and links**: CBOR frames from capsules | `internal/capsule/frame.go` (1 MiB cap, no indefinite lengths, no tags, depth/size limits, duplicate keys refused), `links.go` (broker stamps the caller ID, 64 KiB bodies, 50 calls/s, 5 s timeout, only approved `uses`) | `TestFrameRoundTrip`, `TestFrameTooBig`, `FuzzFrame`, `TestLinks` |
| 5 | **Egress proxy**: a capsule's only way to the network | `internal/capsule/egress.go`: CONNECT only, `[capsule].net` allowlist, DNS resolved by the proxy, private/loopback/link-local/CGNAT/metadata addresses refused, connects to the checked IP, 16 connections | `TestEgressAllows`, `TestEgressProxy`, `FuzzEgress` (never looks up an unlisted host) |
| 6 | **Packages**: `.capsule` files and folders from authors | `internal/capsule/package.go`: zip-slip and link refusal, extraction through `os.Root`, `capsule.lock` hashes every file, store re-verified at every start; `manifest.go` validation; install shows permissions and needs approval again when they change | `TestPackVerifyTamper`, `TestUnzipRejectsEscapes`, `TestManifest`, `FuzzManifest` |
| 7 | **Plugin index**: what `luna capsule install <name>` downloads | `internal/index/index.go`: Ed25519 signature over `index.json` under `[index] public_key`, https only, entries pin the package content hash (checked after download), revocations in the signed file | `TestSignAndParse` (tamper and wrong key refused), `TestFind`, `TestJoin` (no `..`, no absolute or foreign URLs) |
| 8 | **Chat in**: viewers' messages becoming commands | `cmd/luna/plugincmd.go` router: rank → capability check (`internal/perms`), cooldowns, bidi/zero-width stripped, ≤ 21 words; Luna's own messages skipped (`twitchhost.go` echo matching) | `TestCan`, `TestCanOverride`, `TestResolve`, `TestWithRanks`, `TestInvisibleStripped`, `TestEchoes` |
| 9 | **Chat out**: anything sent to Twitch | `internal/guard/guard.go`: control characters flattened, leading `/` or `.` escaped (no chat commands), 500-char cap, RE2 blocklist, rate limit | `TestGuard`, `FuzzClean` |
| 10 | **Credentials** | `internal/secrets`: OS keyring, else AES-256-GCM files (0600) per data dir; tokens never in env, args, config or plugin calls; DeepL key typed without echo (Linux) | `TestFileFallback` |
| 11 | **Control socket**: the CLI and TUI | `cmd/luna/ctl.go`: Unix socket (0600) in the 0700 data folder; `peer_linux.go` checks `SO_PEERCRED` UID | (manual) — **no peer check off Linux** |
| 12 | **OSC**: UDP from VRChat | `internal/osc`: decoder bounds every read, bundles ≤ 4 deep; config refuses non-loopback addresses unless `allow_remote`; avatar IDs from the network must match `avtr_…` before becoming file names | `FuzzDecode`, `TestRoundTrip`, `TestClient` (path refused), `TestOSCStaysLocal` |
| 13 | **Spotify / DeepL / Twitch APIs** | TLS with verification; Spotify login is PKCE with `state`, one-shot listener on 127.0.0.1:43219 for ≤ 5 min; track and playlist IDs from chat must be base62 | `TestLogin` (wrong state refused), `TestClient`, `FuzzIDs` |
| 14 | **Moderation filters**: matching untrusted text | `bundle/moderation.go`: RE2 only (no ReDoS), text folded first (`fold`) so lookalikes don't slip past | bundle tests, `FuzzFold` |

## Run it yourself

```sh
make check test                      # vet, gofmt, every Go/Python/TypeScript test (Linux: needs user namespaces)
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
# each fuzzer, for as long as you like:
go test -run '^$' -fuzz '^FuzzFrame$'    -fuzztime 10m ./internal/capsule
go test -run '^$' -fuzz '^FuzzManifest$' -fuzztime 10m ./internal/capsule
go test -run '^$' -fuzz '^FuzzEgress$'   -fuzztime 10m ./internal/capsule
go test -run '^$' -fuzz '^FuzzDecode$'   -fuzztime 10m ./internal/osc
go test -run '^$' -fuzz '^FuzzClean$'    -fuzztime 10m ./internal/guard
go test -run '^$' -fuzz '^FuzzOSCValue$' -fuzztime 10m ./internal/plugin
go test -run '^$' -fuzz '^FuzzIDs$'      -fuzztime 10m ./internal/spotify
go test -run '^$' -fuzz '^FuzzFold$'     -fuzztime 10m ./bundle
# Windows conformance, on Windows 10 1809+:
go test ./internal/capsule/sandbox/
```

## Results so far (2026-10-12)

- `govulncheck`: found five HTTP/2 vulnerabilities in `golang.org/x/net` v0.57.0 reachable
  through the plugin API's gRPC server (a hostile plugin could crash or exhaust the
  daemon). Fixed by upgrading to v0.60.0; now clean.
- Fuzzing, 20 s each: 1–4 million runs per target. `FuzzIDs` found that a `spotify:track:`
  URI passed any characters through as a track ID (escaped later, so not exploitable);
  IDs must now be base62. The failing input is kept as a regression seed.
- Live checks against real accounts: Twitch writes restored, polls/predictions opened and
  closed, egress from Python and Node capsules (allowed host reached; unlisted host and a
  name resolving to 127.0.0.1 both refused), plugin index tamper refused.

## Known limits (accepted for now)

Marked in the code with `shortcut:` comments; the security-relevant ones:

- **No control-socket peer check off Linux** (`cmd/luna/peer_other.go`); the socket's
  folder is owner-only.
- **Windows backend has never run**; its conformance tests are written, not executed.
- **Egress**: no idle timeout or byte cap per connection (`egress.go`); 16 connections.
- **`capsule.sig` isn't checked**: packages from the index are covered by the index
  signature; hand-passed packages rely on approval plus `capsule.lock`.
- **No rate limit on moderation actions** beyond Twitch's own (`guard.go`).
- **Output blocklist doesn't fold lookalike characters** (`guard.go`); the moderation
  filters on incoming chat do.
- **Two plugin-side rank checks** (Points' `!points add/remove`, Queue's `!queue open`)
  mirror the default capability table instead of reading the streamer's (the plugin API
  doesn't expose it yet).
- **Capability changes and same-rank override**: `perms.same_rank_override` is accepted
  in config but nothing reads it; undo never lets equal ranks override each other.
- **Secrets on Windows**: the DeepL key prompt echoes.
- **Shared runtimes (Python, Node) are Linux-only**; Windows runs `luna` and `native`
  plugins.

## Out of scope

Kernel exploits and hardware side channels, a compromised host or user account, and
other apps running as the same user (they can already read the user's files and send
OSC to Luna).
