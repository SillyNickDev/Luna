# Performance

A full Luna setup (core plus five first-party plugins) should idle under 60 MB of RAM and near-zero CPU, and add under 2 ms between Twitch sending an event and a plugin seeing it. Every number below is a release gate, checked in CI.

## Budgets

These are starting targets; the first benchmarks will confirm or adjust them, but a release that misses one needs a written reason.

| Metric | Budget | Measured on |
| --- | --- | --- |
| Core daemon idle memory (RSS) | ≤ 30 MB | Connected to Twitch, OBS, OSC, HR; no capsules |
| Core idle CPU | < 0.1% of one core | Same, averaged over 10 min |
| First-party plugin bundle | ≤ 10 MB | Essentials, Moderation, Events, Shoutouts |
| Extra memory per capsule: native | ≤ 3 MB | Hello world |
| Extra memory per capsule: wasm | ≤ 8 MB | Hello world |
| Extra memory per capsule: Python | ≤ 20 MB | Hello world |
| Extra memory per capsule: Node | ≤ 40 MB | Hello world |
| Event overhead, receive → handler | p99 < 2 ms | Excluding network, under 200 events/s |
| Reply overhead, handler → Helix request | p99 < 1 ms | Excluding Twitch rate limits |
| Cold start to ready | < 300 ms | Excluding the Twitch handshake |
| Capsule cold start | native < 50 ms, Python < 300 ms | To `ready` |
| Binary size | ≤ 25 MB | Stripped, per platform |
| Disk growth | ≤ 5 MB per stream hour | Event log at default retention |

The reference machine is a mid-range laptop: a 4-core CPU from the last five years, 8 GB RAM and an SSD. Luna must also run acceptably on a Raspberry Pi 5, which acts as the low-end check.

## Principles

1. **Idle means idle**: no polling loops. Everything is event-driven, and all timers share one timer heap with a single wake-up.
2. **Do the work once**: decode each event once, encode it once, and hand the same bytes to every subscriber.
3. **Filter at the source**: an event a plugin doesn't want never crosses its socket. Subscriptions and decorator filters are evaluated in the core.
4. **Latest value wins for streams**: high-rate signals (heart rate, OSC parameters, tracking) are coalesced, so a slow consumer gets the newest value, not a growing backlog.
5. **Pay only for what's installed**: integrations and capsules nobody uses are never started.
6. **Bounded everything**: every queue, buffer, cache and log has a size cap and a defined overflow behaviour. Memory never grows with stream length.
7. **Measure before optimising**: no change goes in for speed alone without a benchmark showing the gain.

## Event hot path

&#91;embedded content: event hot path · per-hop budgets\]

Per-hop numbers are budgets for `luna debug trace`; together they leave headroom inside the 2 ms and 1 ms targets. The SDK hop is the largest because it includes language runtime overhead, which is why high-rate plugins should use native or wasm.

## Core daemon

### Go runtime

- **Soft memory limit**: set `GOMEMLIMIT` from config (default 48 MiB) so the garbage collector works harder near the budget instead of letting the heap grow.
- **Allocation discipline** on the hot path: pooled buffers (`sync.Pool`) for frames, pre-sized slices, no `fmt` or reflection-based JSON per event. The decoders are generated code.
- **Few goroutines**: one per connection and one per capsule, not one per event. Goroutine count is a tracked metric.
- **Profile-guided optimisation**: build releases with PGO from a profile captured during the stream-replay benchmark.

### Event bus

- One lock-free ring buffer (default 4,096 events) that every subscriber reads from with its own cursor, so publishing never waits on readers.
- A subscriber that falls a full ring behind gets a `lagged` notice and skips ahead; it never makes Luna buffer without limit.
- The subscription index is a map from event type to subscribers, so dispatch cost depends on interested plugins, not installed ones.

### Build

- Pure Go, no cgo: `CGO_ENABLED=0`, `-trimpath`, `-ldflags="-s -w"`. Static binaries cross-compile from one machine.
- Integrations compile in but initialise lazily; an unconfigured OBS client costs no memory.
- The capsule runtime's per-OS backends sit behind build tags, so each binary contains only its own platform's code.

## Capsules

Capsules are where memory goes, so the runtime avoids running them when it can and keeps each one small when it must.

- **First-party bundle**: the core bundle (Essentials, Moderation, Events, Shoutouts) ships as one native Go capsule hosting all four. That's one process instead of four, and it's still isolated from the core. Third-party plugins always get their own capsule.
- **Lazy start**: a capsule starts on the first event, command or link call it subscribed to, not at boot. Chat commands look instant because the core answers cooldown and permission checks first, while the capsule starts in parallel.
- **Hibernation**: a capsule idle for 15 minutes (configurable, or `hibernate = false` in the manifest) is stopped cleanly and restarted on demand. `store` data lives in the core, so nothing is lost. Capsules with timers or link providers in use never hibernate.
- **Shared runtimes**: one interpreter install on disk, mapped read-only, so the OS shares code pages across capsules (Capsule spec). Python capsules also share a precompiled `.pyc` cache for the runtime's standard library.
- **Python start-up**: run with `-I -S` (isolated, no site import) plus a frozen SDK, which cuts tens of milliseconds and several MB per capsule.
- **Pick the cheapest runtime**: `luna capsule init` notes the per-runtime budgets, and templates for high-rate plugins (tracking, heart rate) default to native or wasm.
- **Per-capsule caps** default to 64 MB memory and 0.25 CPU (Capsule spec), so one hungry plugin can't push the whole setup over budget.

## Storage, network and integrations

### Storage (SQLite)

- WAL mode, `synchronous=NORMAL`, one writer goroutine, and prepared statements for every query.
- The event log is written in batches (every 250 ms or 500 events, whichever comes first) in one transaction.
- Plugin `store` reads come from an in-memory cache with write-through, capped per plugin.
- Retention: event log 30 days by default, compacted nightly when idle. `VACUUM` runs only when free pages pass 25%.

### Twitch

- One EventSub WebSocket for all subscriptions. Reconnects honour Twitch's reconnect message so no events are lost.
- One pooled HTTP client with keep-alive to Helix, so sending chat reuses a warm connection rather than a new TLS handshake.
- Chat goes through a token-bucket limiter matching the bot's role limits; bursts queue briefly instead of failing.

### Integrations

- **OSC**: UDP with a reused socket. Outgoing parameters are coalesced per address per frame (default 30 Hz), and incoming ones update a parameter cache that `bot.osc.param` reads with no round trip.
- **OBS**: subscribe only to the obs-websocket event categories some plugin needs (the protocol lets clients choose), so OBS doesn't stream unused events.
- **Heart rate**: coalesced to latest-value; `hr.update` is delivered at most 4 times a second by default even if the band reports faster.

## Measuring and enforcing

### Built into Luna

| Command | Shows |
| --- | --- |
| `luna status --perf` | Core RSS and CPU, per-capsule RSS/CPU/state, bus lag, event p50/p99 over the last 10 min |
| `luna capsule inspect <name>` | That capsule's memory against its budget and cap, start time, hibernation count |
| `luna debug profile [cpu\|heap] --for 30s` | A Go pprof profile of the core, written to a file |
| `luna debug trace <event-id>` | Time spent at each hop for one event |

An opt-in local metrics endpoint (Prometheus format, `127.0.0.1` only) feeds a homelab Grafana for anyone who wants graphs.

### CI gates

- **Micro-benchmarks** for decode, dispatch, encode and the store cache; a regression over 10% fails the build.
- **Stream replay benchmark**: a recorded 3-hour stream with chat, raids, redeems and a heart rate feed, replayed at 10× speed against the full first-party bundle. It checks every budget in the table above.
- **Soak test** nightly: 24 hours of simulated events. RSS at hour 24 must be within 5% of hour 1, which catches leaks.
- **Size check**: binary and `.capsule` package sizes reported on every pull request.
- **Pi run**: the replay benchmark also runs on a Raspberry Pi 5 before each release.

Budget results publish with each release, so anyone can see how Luna performs rather than taking "lightweight" on trust.
