# Luna — Spec & Plugin API

Oct 10, 2026 · @Nick

## Overview

A self-hosted, open-source Twitch bot that treats your stream as one event bus: chat, Twitch events, your VR avatar, your heart rate and OBS all feed in, and plugins react to any of them. **Name: Luna. Its plugins run as Luna Capsules**.

**Goals**

- Runs entirely on your own machine or homelab. No cloud account, no subscription, no telemetry.
- One luna binary, Linux-first, also on Windows and macOS.
- Everything beyond the core is a plugin, including the first-party plugins.
- Each plugin runs in its own isolated capsule, declares what it can touch, and restarts on its own if it fails, so installing someone else's plugin is safe.&#32;
- Bridges the stream to VRChat and Resonite, which no mainstream bot does natively.

* **Lightweight**: core under 30 MB idle and under 2 ms of event overhead, enforced as release gates.
* **Secure by default**: chat, plugins and the network are untrusted, and common exploit primitives are designed out.
* **Easy to extend**: a five-line plugin in Python, TypeScript, Go or Rust, tested offline.

**Non-goals (v1)**

- Hosted multi-tenant service for other streamers. Others self-host their own copy.
- Multi-platform chat (YouTube, Kick). Keep the event model platform-neutral so it can be added later.
- A web dashboard as the main UI. Config files first, a small local web UI later.

**The spec at a glance**

| Tab | Covers |
| --- | --- |
| This tab | Goals, differentiators, architecture, plugin model summary, roadmap |
| Luna Capsules Spec | Plugin isolation: package format, runtimes, OS backends, links, supervision |
| Luna CLI | `luna setup`, management commands, stream tools, plugin-defined commands |
| Luna SDK | Writing plugins: API, events, commands, config, testing |
| First-party plugins | The 14 built-in plugins, from essentials and moderation to giveaways and VR |
| Permissions | Ranks from broadcaster to viewer, override rules, Luna mods, capabilities |
| Performance | Budgets and how they're met and enforced |
| Security | Threat model, exploit primitives, secrets, supply chain |

## What sets it apart

Nightbot, StreamElements and Fossabot are cloud chat bots; Streamer.bot is powerful but Windows-only and closed source. The gap is a local, open, VR-aware bot with a real developer story.

| Feature | Why other bots don't have it |
| --- | --- |
| **VRChat / Resonite bridge** over OSC: redeems and bits flip avatar parameters (toggles, outfits, props), chat goes to the in-game chatbox, avatar state comes back as events | Cloud bots can't reach your local VR session; existing OSC tools are one-off scripts |
| **Biometric events** from HRCollect: `!hr` command, "heart rate spiked" triggers, HR on overlays and in the chatbox | Pulsoid/HypeRate need a separate paid service and glue |
| **Unified event bus**: one stream of typed events from Twitch, OSC, OBS, HR and timers, so a plugin can say "when HR > 140 during a horror game, and a sub comes in, play X" | Most bots only react to chat and Twitch alerts |
| **Event simulator and replay**: fire fake subs, raids and redeems offline, or replay a recorded stream's events to test plugins | Testing on other bots means testing live |
| **Isolated plugins in any language** (one lightweight capsule each) with declared permissions, brokered links and auto-restart | Bot plugins are usually unsandboxed scripts or a single language |
| **Local-first data**: per-viewer memory (first seen, watch streaks, notes), all in one SQLite file you own and can export | Viewer data lives on someone else's server |
| **Hot reload**: edit a plugin or config, it reloads without dropping the Twitch connection | Restart-the-bot workflows |
| **Mod action timeline with undo**: every timeout, ban, deletion and bot action logged with one-click reverse | Twitch's mod log has no undo and misses bot context |
| Rank ladder with Luna mods: head mods, signed-in Luna mods and override rules with locks | Other bots have a flat mod role; Twitch's API doesn't expose Lead Moderators at all |
| Provably fair giveaways: committed seed, verifiable draw | Other bots ask chat to trust the result |
| Published performance budgets: memory, CPU and latency checked on every release | "Lightweight" is usually a claim, not a measurement |
| Security by design: tokens never reach plugins, exploit primitives designed out, signed everything | Most bots run plugins with full access to tokens and the machine |

The first three are the headline. They come straight from your existing setup (VRChat, HRCollect, obs-websocket), so the bot ties projects you already have into one thing other creators can run.

## v1 scope

v1 is everything a streamer needs on day one: the core bundle plus the most-used optional plugins, all built as capsules on the public SDK so the plugin system is proven from the start.

- [ ] Core: Twitch connection, event bus, storage, ranks and permissions, output guard
- [ ] Capsule runtime with the Linux backend; Windows right after
- [ ] `luna` CLI: `setup`, management commands, stream tools
- [ ] Python SDK with the test harness; TypeScript and Go next
- [ ] Core bundle: Essentials, Moderation, Events, Shoutouts
- [ ] Optional at launch: Giveaways, Points, Stream tools, Avatar, Heart rate
- [ ] Simulator and event replay

Later: Games, Polls, Queue, Song requests, Accessibility, the Rust SDK, the macOS backend, the Resonite bridge and a local web UI.

## Architecture

Adapters turn each outside system into typed events on one bus; plugins only ever talk to the core, never to Twitch or OSC directly.

&#91;embedded content: architecture · 5 adapters, core, 2 plugin tiers\]

Every outbound action (chat, OSC, OBS, mod) passes through the output guard, so rate limits and the blocklist apply no matter which plugin asked.

**Stack**

- **Go**, single static binary. Same language as your VTubing app; cross-compiles to Linux and Windows easily.
- **Twitch**: EventSub over WebSocket for events (including chat messages) and the Helix API for sending chat and mod actions, so no public webhook URL is needed. IRC still works but treat it as legacy; keep it behind the adapter interface either way. Check Twitch's current docs for scopes before building.
- **Bus**: in-process pub/sub, one buffered queue per subscriber so a slow plugin can't stall the others. Every event is also appended to an event log table, which is what replay reads from.
- **Storage**: SQLite via a pure-Go driver (modernc.org/sqlite), so no cgo.
- **Plugins**: one capsule each, run by the bot's own Capsule runtime (no Podman or Docker needed); the core talks to each over its own Unix socket using gRPC.
- **Libraries**: go-osc for OSC, goobs for obs-websocket v5.

## Plugin API

Every plugin runs in its own capsule and talks to the core over its own Unix socket. A crash, hang or failed health check restarts only that plugin's capsule; the bot and the Twitch connection stay up. Plugins can be written in any language, using an SDK or plain gRPC.

### Why capsules

| Option | Pros | Cons |
| --- | --- | --- |
| In-process WASM (Extism) | Light, fast to start | A hung plugin runs inside the bot's process; no native libraries or hardware |
| Embedded Lua or JS | Easy to write | One language, sandboxing is on you |
| Podman/Docker containers | Mature isolation | A full image per plugin, a daemon or VM on Windows/macOS, heavy for small plugins |
| **Capsules (own spec, Podman-inspired)** | Process isolation with no image layers or VM; shared runtimes; native on Linux, Windows and macOS | We build and maintain the runtime; isolation strength differs per OS |

Recommendation: Luna Capsules, defined in their own spec below and run by the bot's own runtime. Network is off for every plugin unless its manifest asks for it.

A Luna Capsule is a plain OS process wrapped in the host's native isolation (namespaces on Linux, AppContainer on Windows, Seatbelt on macOS, with wasm as the portable fallback), with shared runtimes to keep memory low. The full specification is in Luna Capsules Spec; how to write plugins is in Luna SDK.

### Manifest (`plugin.toml`)

```toml
id = "dev.sable.hrc"
name = "Heart rate alerts"
version = "0.1.0"
api = "1"

[capsule]
runtime = "python-3.12"   # or "native", "wasm", "node-22"
entry = "main.py"
limits = { memory = "64m", cpu = 0.25 }
net = "none"

[permissions]
events = ["hr.update", "twitch.chat.message"]
chat = ["send"]
osc = ["send:/avatar/parameters/Panic"]
store = true

[links]
provides = ["spike"]
uses = ["dev.sable.overlay:show"]

[cli]
name = "hrc"                # luna hrc <command>

[[chat.commands]]
name = "hr"
capability = "chat.commands.use"

[config]
threshold = { type = "int", default = 140 }
```

Most of this is generated: `luna capsule dev` writes commands, events and config from the code (Luna SDK tab), leaving limits and permissions for you.

Installing with luna capsule install \<name> (for example luna capsule install hrc) shows these permissions for approval, and Luna refuses any call outside them.

### Lifecycle

Capsules start lazily on their first event, command or link call, hibernate after 15 idle minutes, and restart alone with backoff if they fail. The state machine and restart policy are in the Capsule Spec tab.

### Event envelope

```json
{
  "id": "01J9X…",
  "type": "twitch.channel.redeem",
  "source": "twitch",
  "ts": "2026-10-10T13:02:11Z",
  "simulated": false,
  "data": { "user": { "id": "123", "login": "viewer", "roles": ["sub"] }, "reward": "Toggle ears", "input": "" }
}
```

Event families: `twitch.*`, `osc.*` (avatar parameter changes), `obs.*`, `hr.*`, `timer.*`, `bot.*` (startup, plugin loaded). Types are versioned with the API; new fields are additive only.

### Host API

Plugins call Luna over their host socket (gRPC with published `.proto` files). The SDKs wrap this as one `bot` object with `chat`, `mod`, `store`, `viewers`, `osc`, `obs`, `hr`, `http`, `timers`, `links` and more. Each group maps to one manifest permission, and every call is checked against the plugin's permissions and the triggering user's rank.

### Example (Python SDK)

```python
from luna import bot, Config

class Settings(Config):
    threshold: int = 140

bot.use_config(Settings)

@bot.on("hr.update", when=lambda ev: ev.bpm > bot.config.threshold)
def spike(ev):
    if bot.store.get("cooldown"):
        return
    bot.chat.send(f"Winterpaw's heart rate just hit {ev.bpm} bpm 💓")
    bot.osc.send("/avatar/parameters/Panic", True)
    bot.store.set("cooldown", 1, ttl=120)

@bot.chat_command("hr", cooldown="10s")
def hr(ctx):
    ctx.reply(f"{bot.hr.bpm} bpm")

bot.run()
```

### Links and supervision

Plugins never talk to each other directly. A link is a named channel the core brokers, opened only when both sides declare it and the user approved it; the SDK makes it feel like a function call. A failing capsule is restarted alone with backoff and marked failed after five failures in ten minutes, and its `store` data survives because it lives in the core.

## Security, permissions and performance

These three are cross-cutting rules every part of Luna follows. Each has its own tab.

- **Security** (Security): tokens live only in the core and never reach plugins; there are no network listeners; exploit primitives such as injection, path traversal, SSRF and ReDoS are designed out; updates and packages are signed.
- **Permissions** (Permissions): five ranks from broadcaster to viewer, with Luna mods above plain mods. A higher rank overrides a lower one, never the reverse, and every action is attributed to a real person.
- **Performance** (Performance): core under 30 MB idle and under 2 ms of event overhead, with lazy-start and hibernating capsules. Budgets are checked by CI on every release.

**Config** is one `luna.toml` plus per-plugin sections, hot-reloaded and validated before applying. An invalid file keeps the last good config running.

## Roadmap and open questions

Each phase is usable on stream before the next starts, and every phase must pass the Performance budgets and the Security checks before release.

1. **Core**: Twitch auth and EventSub, event bus, SQLite, config hot reload, ranks and permissions, output guard, simulator.
2. **Luna Capsules**: runtime, Linux backend, links, supervision, conformance suite; `luna capsule` commands.
3. **CLI and SDK**: `luna setup` and management commands; Python SDK with test harness; manifest generation.
4. **Core bundle**: Essentials, Moderation, Events, Shoutouts, running as one native capsule.
5. **Launch plugins**: Giveaways, Points, Stream tools, Avatar, Heart rate. Windows backend, TypeScript and Go SDKs. **v1 release.**
6. **After v1**: Games, Polls, Queue, Song requests, Accessibility, plugin index, Rust SDK, macOS backend, Resonite bridge, local web UI, external security audit before 1.0.

**Open questions**

- [ ] Whether Luna ships under Sable Studios or as a standalone open-source project, and whether Luna Capsules gets its own repo.
- [ ] Licence: MIT/Apache-2.0 for maximum adoption, or GPL-3.0 to keep forks open.
- [ ] Plugin index: who hosts it, how signing and revocation work.
- [ ] Head mods: Twitch's API doesn't expose Lead Moderators, so detection relies on the badge plus manual setting. Watch for API support.
- [ ] Resonite bridge transport: what Resonite exposes for external input.
- [ ] macOS capsule backend strength, given Seatbelt's deprecation.
