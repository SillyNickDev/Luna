# Performance & Budgets

In live streaming, every megabyte of RAM and every millisecond of CPU time competes with the game, OBS, SteamVR and tracking. Luna has budgets for the things that matter, and this page says which are measured and which are still targets. Nothing checks them automatically yet.

---

## 1. Budgets

| Metric | Budget | Measured (Linux, x86-64) |
| --- | :---: | --- |
| **Core daemon memory (RSS)** | $\le 30\text{ MB}$ | 26 MB connected to Twitch with the core bundle; 25 MB with OSC on |
| **Core bundle** (Essentials, Moderation, Events, Shoutouts) | $\le 10\text{ MB}$ | 3.8 MB private memory (its code pages are shared with the daemon) |
| **Other first-party plugins** (Points, Giveaways, Stream, Avatar, Heart rate) | — | about 3.5 MB private memory each |
| **Extra memory: Python capsule** | $\le 20\text{ MB}$ | not measured yet |
| **Extra memory: Node.js capsule** | $\le 40\text{ MB}$ | about 33 MB |
| **Static binary size** | $\le 25\text{ MB}$ | about 23 MB (the TUI added 3) |
| **Event overhead (receive → handler)** | $\text{p99} < 2\text{ ms}$ | not measured yet |
| **Capsule cold start to ready** | native $< 50\text{ ms}$, Python $< 300\text{ ms}$ | not measured yet |

---

## 2. What Keeps It Small

- **Idle means idle**: no polling loops in the core; it sleeps until a socket or a timer wakes it.
- **Filter at the source**: a plugin only gets the event types it subscribed to (and its manifest allows); everything else stays in the core.
- **Slow consumers drop, never stall**: each subscriber has its own bounded queue on the bus. A plugin that falls behind loses events (counted as lagged) instead of making the daemon buffer without limit.
- **Latest value wins for OSC**: outgoing avatar parameters are coalesced per address into 30 Hz frames, and incoming ones become `osc.param` events at most once per frame. VRChat's high-rate built-ins (velocity, gesture weights, visemes) stay out of the bus.
- **Soft memory ceiling**: the daemon sets Go's `GOMEMLIMIT` from `memory_limit_mib` (default 48 MiB), so the garbage collector works harder near it instead of growing the heap.

---

## 3. Capsules

- **One process for the core bundle**: Essentials, Moderation, Events and Shoutouts run as one native capsule, and every first-party plugin runs on the `luna` runtime: the daemon's own binary, so the operating system shares its code pages.
- **Shared runtimes**: `python-3.12` and `node-22` are installed once and mounted read-only into every capsule using them, so their code pages are shared too.
- **Python** runs with `-I -S` (isolated, no site packages).
- **Node** runs with a small young generation and one V8 worker thread (`--max-semi-space-size=1 --v8-pool-size=1`), which takes an idle capsule from about 42 MB to 24 MB.
- **Per-capsule caps** default to 64 MB of memory and a quarter of one CPU, so one hungry plugin can't push the whole setup over budget.
- Capsules start with the daemon and run until stopped; lazy start and hibernation aren't built yet.

---

## 4. Storage

- **SQLite in WAL mode**, so reads don't wait for writes.
- **Batched event log**: events are written in batches of up to 500, or every 250 ms, in one transaction. Live avatar state (`osc.param`) isn't logged.
- **No retention yet**: the event log keeps every other event, so it grows with chat and stream events.

---

## 5. Inspecting

```sh
luna status                 # uptime, Twitch, chat rate-limit headroom, capsules, OSC
luna capsule inspect <name> # a capsule's limits, permissions and failure history
```

For memory, read a capsule's `Private_Dirty` from `/proc/<pid>/smaps_rollup`: `Rss` also counts the code pages it shares with the daemon and other capsules.
