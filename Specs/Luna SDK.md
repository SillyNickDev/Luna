# Luna SDK

A working plugin is five lines, runs against fake events without a stream or a Twitch account, and every error message tells you the exact fix.

## Design goals

- **Five-line hello world**: no classes to subclass, no boilerplate, no manual manifest for the basics.
- **Code is the source of truth**: decorators declare events, commands and links; `luna capsule dev` writes them into the manifest for you. You only edit the manifest for limits and permissions.
- **Fails with the fix**: a missing permission raises an error that names the exact manifest line to add. Bad config names the key, the expected type and the value it got.
- **Typed everywhere**: event payloads, config and command args are typed, so editors autocomplete them. Command args come straight from function signatures.
- **Same shape in every language**: `bot.chat.send`, `bot.store.get`, `@on("hr.update")` read the same in Python, TypeScript, Go and Rust, adjusted only for each language's idioms.
- **Offline by default**: `luna capsule dev` and the test harness run the plugin against the simulator. Nothing reaches Twitch unless you pass `--live`.
- **Sync or async, your choice**: plain functions and async functions both work as handlers.

## Quick start

```sh
luna capsule init hello --runtime python-3.12   # or node-22, native-go, native-rust
cd hello
luna capsule dev                                # runs it against the simulator, reloads on save
luna sim follow --user testviewer               # in another terminal
```

The scaffold is the hello world below, a test file, and a manifest with safe defaults. The same plugin in each first-party SDK:

**Python**

```python
from luna import bot

@bot.on("twitch.follow")
def welcome(ev):
    bot.chat.send(f"Welcome to the pack, {ev.user.name}!")

bot.run()
```

**TypeScript**

```ts
import { bot } from "@luna/sdk";

bot.on("twitch.follow", (ev) => {
  bot.chat.send(`Welcome to the pack, ${ev.user.name}!`);
});

bot.run();
```

**Go**

```go
package main

import "github.com/sable/luna-go"

func main() {
    bot := luna.New()
    bot.On("twitch.follow", func(ctx luna.Ctx, ev luna.Follow) error {
        return bot.Chat.Send(ctx, "Welcome to the pack, "+ev.User.Name+"!")
    })
    bot.Run()
}
```

**Rust**

```rust
use luna::prelude::*;

#[luna::main]
async fn main(bot: Bot) -> Result<()> {
    bot.on("twitch.follow", |ev: Follow| async move {
        bot.chat().send(format!("Welcome to the pack, {}!", ev.user.name)).await
    });
    bot.run().await
}
```

When it works: `luna capsule pack` to build it, `luna capsule install ./hello` to run it for real.

## Dev loop

&#91;embedded content: plugin dev loop · 5 commands\]

Each box is a `luna capsule` subcommand. Most time is spent cycling between `dev` and `test`, both fully offline.

## API surface

Everything hangs off one `bot` object, grouped by what it touches; each group maps to one manifest permission, so the permission you need is the name you just typed. Python names shown; other SDKs use their language's casing.

| Group | Main calls | Permission |
| --- | --- | --- |
| `bot.on` / `bot.once` / `bot.wait_for` | Event handlers and awaiting events | `events` (inferred from handlers) |
| `bot.cli` / `bot.chat_command` | Declare CLI and chat commands | none (declared) |
| `bot.chat` | `send`, `reply`, `whisper`, `announce` | `chat` |
| `bot.mod` | `timeout`, `ban`, `unban`, `delete`, `warn` | `mod` |
| `bot.store` | `get`, `set(ttl=)`, `delete`, `incr`, `keys(prefix)`, `transaction()` | `store` |
| `bot.viewers` | `get(user)`, `top(by="watch_streak")`, `note(user, text)` | `viewers` |
| `bot.config` | Typed access to the plugin's config | none |
| `bot.osc` | `send(address, value)`, `chatbox(text)`, `param(name)` | `osc` (address allowlist) |
| `bot.obs` | `scene(name)`, `source(name).show()`, `call(request, data)` | `obs` |
| `bot.hr` | `bpm` (latest), `history(minutes)` | `events: hr.*` |
| `bot.http` | `get`, `post`, `fetch` | `net` allowlist |
| `bot.timers` | `after`, `every`, `cron`, `cancel` | none |
| `bot.links` | `provide`, `use(...).call` | `links` |
| `bot.emit` | Publish a `plugin.<id>.*` event | none |
| `bot.log` | `debug`, `info`, `warn`, `error` | none |
| `bot.lifecycle` | `on_start`, `on_stop` hooks | none |

## Events

Handlers subscribe by event type, filter by payload fields in the decorator, and get a typed event plus a context for replying.

```python
# filter in the decorator instead of an if at the top
@bot.on("twitch.redeem", reward="Toggle ears")
def ears(ev):
    bot.osc.send("/avatar/parameters/Ears", not bot.osc.param("Ears"))

# wildcards and predicates
@bot.on("twitch.*", when=lambda ev: ev.user and ev.user.is_mod)
def mod_activity(ev): ...

# fire once, then unsubscribe
@bot.once("bot.start")
def hello(ev):
    bot.chat.send("Luna is up")

# wait for the next matching event inside a handler
@bot.chat_command("duel")
async def duel(ctx, target: str):
    ctx.reply(f"{target}, type !accept within 30s")
    ok = await bot.wait_for("twitch.chat.message", user=target, text="!accept", timeout=30)
    ctx.reply("Fight!" if ok else f"{target} chickened out")
```

- **Typed payloads**: `ev` is a `Follow`, `Redeem`, `HrUpdate`, etc. Unknown fields stay reachable as `ev.raw`.
- **Common fields** on every event: `ev.type`, `ev.id`, `ev.ts`, `ev.source`, `ev.simulated`.
- **Context**: handlers that take a `ctx` get `ctx.reply`, `ctx.user` and `ctx.event`. Handlers that don't, don't need it.
- **Composite triggers** read as one expression: `@bot.on("twitch.sub", when=lambda ev: bot.hr.bpm > 140)` runs only when a sub lands while your heart rate is high.
- **Event names** come from a generated constants module (`from luna.events import TWITCH_FOLLOW`) if you prefer autocomplete to strings.

## Commands, config and state

### Commands from signatures

The function signature is the command's argument list. Type hints become arg types, defaults make args optional, and the docstring becomes the help text. `luna capsule dev` writes the `[[cli.commands]]` and `[[chat.commands]]` entries for you.

```python
@bot.cli("threshold", mutates=True)
def threshold(bpm: int | None = None, reset: bool = False):
    """Get or set the heart rate alert threshold."""
    if reset:
        bot.store.delete("threshold")
    elif bpm is not None:
        bot.store.set("threshold", bpm)
    return {"threshold": bot.store.get("threshold", default=bot.config.threshold)}

@bot.chat_command("hr", aliases=["heartrate"], cooldown="10s", user_cooldown="30s")
def hr(ctx):
    ctx.reply(f"{bot.hr.bpm} bpm")
```

Returning a dict or list from a CLI command prints a table (or JSON with `--json`); returning a string prints it as is.

### Typed config

```python
from luna import Config

class Settings(Config):
    threshold: int = 140
    message: str = "Heart rate just hit {bpm} bpm"
    cooldown_s: int = 120

bot.use_config(Settings)   # bot.config.threshold is an int, with autocomplete
```

The class generates the `[config]` schema, so `luna config set plugins.hrc.threshold 150` is validated before the plugin sees it. Config changes arrive live through an `on_config_change` hook.

### Store

- Key-value, private to the plugin, persisted by Luna so it survives restarts and reinstalls (unless uninstalled without `--keep-data`).
- Values are JSON-serialisable; `bot.store.model("stats", Stats)` gives a typed, auto-saved object instead of raw keys.
- `set(key, value, ttl=120)` for cooldowns; `incr("deaths")` is atomic; `with bot.store.transaction():` groups writes.

### Viewers

`bot.viewers.get(user)` returns Luna's shared viewer profile: first seen, last seen, watch streak, chat count, plus a plugin-private `extra` dict for your own per-viewer data.

## Links, timers and integrations

### Links feel like function calls

The provider exposes a typed function; the consumer gets a typed client. The SDK handles framing, the broker, timeouts and `link_unavailable`.

```python
# in dev.sable.overlay
@bot.links.provide("show")
def show(text: str, seconds: int = 5) -> bool:
    ...
    return True

# in dev.sable.hrc
overlay = bot.links.use("dev.sable.overlay")

@bot.on("hr.update", when=lambda ev: ev.bpm > bot.config.threshold)
def spike(ev):
    try:
        overlay.show(text=f"{ev.bpm} bpm")
    except bot.LinkUnavailable:
        bot.log.warn("overlay is restarting, skipped")
```

`luna capsule pack` on the provider exports the function signatures, so the consumer's editor autocompletes `overlay.show` once the provider package is listed as a dev dependency.

### Timers

```python
@bot.timers.every("15m", only_if_chat_active=True)
def hydrate():
    bot.chat.send("Hydration check 💧")

bot.timers.after("30s", lambda: bot.osc.send("/avatar/parameters/Panic", False))

@bot.timers.cron("0 20 * * FRI")   # Luna's local time zone
def friday(): ...
```

Timers are owned by Luna, not the capsule, so they survive a capsule restart and never drift.

### Integrations

- **OSC**: `bot.osc.param("Ears")` reads the last value VRChat reported; `@bot.on("osc.param", name="Ears")` fires when it changes; `bot.osc.chatbox(text)` handles VRChat's chatbox length and rate limits for you.
- **OBS**: high-level helpers for the common jobs (`scene`, `source(...).show()`, `.hide()`, `record.start()`), and `bot.obs.call(...)` for any raw obs-websocket request.
- **Heart rate**: `bot.hr.bpm` is always the latest value, so most plugins never subscribe to `hr.update` at all.

## Concurrency and errors

### Concurrency

- **Ordering**: each handler receives its events in order. Different handlers run concurrently.
- **Sync handlers** run on a worker pool, so a slow sync handler never blocks the event loop. Async handlers run on the loop.
- **Per-handler limits**: `@bot.on(..., concurrency=1)` serialises a handler; `drop_if_busy=True` skips events instead of queueing (good for high-rate `hr.update`).
- **Language idioms**: Python asyncio, TypeScript promises, Go goroutines with `context.Context` (cancelled on shutdown), Rust async on tokio.

### Errors

An exception in a handler is logged with the event that caused it, and the plugin keeps running. It never crashes the capsule unless you set `bot.crash_on_error = True`, which is useful in tests.

| Error | When | What the message says |
| --- | --- | --- |
| `PermissionDenied` | Calling an API the manifest doesn't allow | The exact line to add, e.g. `add "send:/avatar/parameters/Ears" to [permissions].osc` |
| `RateLimited` | Chat or a link over its limit | `retry_after` in seconds; `bot.chat.send(..., wait=True)` waits for you |
| `LinkUnavailable` | Provider is restarting or failed | Provider ID and its state |
| `ConfigError` | Bad config at load or change | Key, expected type, value received |
| `NotConnected` | OBS, OSC or HR source offline | Which integration, and `luna integrations test <name>` |
| `Timeout` | A call or `wait_for` ran out of time | The call and the limit |

Every error is a subclass of `bot.LunaError`, so one `except` catches them all.

## Testing

Every SDK ships a test harness that runs the plugin in-process against a fake Luna, with no daemon, capsule or Twitch account. It plugs into the language's normal test runner (pytest, Vitest, `go test`, `cargo test`).

```python
from luna.testing import harness
import plugin  # your main.py

def test_spike_alerts_once():
    t = harness(plugin, config={"threshold": 140})
    t.emit("hr.update", bpm=150)
    t.emit("hr.update", bpm=155)          # inside the cooldown
    assert t.chat.sent == ["Winterpaw's heart rate just hit 150 bpm 💓"]
    assert t.osc.sent[-1] == ("/avatar/parameters/Panic", True)

def test_cooldown_expires():
    t = harness(plugin)
    t.emit("hr.update", bpm=150)
    t.clock.advance("2m")                  # fake time, instant
    t.emit("hr.update", bpm=150)
    assert len(t.chat.sent) == 2

def test_threshold_command():
    t = harness(plugin)
    assert t.cli("threshold", "160") == {"threshold": 160}
    assert t.chat_command("!hr", user="viewer").replies == ["0 bpm"]
```

- **Fakes for everything**: `t.chat`, `t.osc`, `t.obs`, `t.http` record calls; `t.http.mock("api.example.com", json=...)` stubs responses.
- **Fake clock**: timers, TTLs, cooldowns and `wait_for` timeouts advance instantly with `t.clock.advance`.
- **Links**: `t.link("dev.sable.overlay", show=lambda **kw: True)` fakes a provider; `t.link_down(...)` tests the fallback path.
- **Permissions are enforced** in tests too, so a missing manifest entry fails the test rather than surprising you after install.
- **Recorded sessions**: `t.replay("sessions/2026-10-09.lunarec")` runs a real stream's events through the plugin and asserts nothing raised.

## Languages, versioning and the wire protocol

| SDK | Package | Status |
| --- | --- | --- |
| Python | `luna` on PyPI | First-party, first to ship (the reference SDK) |
| TypeScript / JavaScript | `@luna/sdk` on npm | First-party |
| Go | `github.com/sable/luna-go` | First-party (shares code with Luna itself) |
| Rust | `luna` on crates.io | First-party, after the others |
| Anything else | Write against the wire protocol | Community |

- **Versioning**: SDKs follow semver. The plugin API version (`api = "1"`) is separate; Luna supports the current and previous API versions, so a Luna update never breaks an installed plugin overnight.
- **Additive changes only** within an API version: new events, fields and calls may appear; nothing is removed or renamed.
- **Wire protocol**: the host socket speaks gRPC with published `.proto` files. Any language with gRPC can write a plugin; the SDKs are convenience layers that add decorators, typing, the fake clock and the test harness.
- **Docs**: generated from the `.proto` files and SDK docstrings, with every example in this tab kept as a tested file in the SDK repos so the docs can't drift from the code.
- **Templates**: `luna capsule init` scaffolds from a template repo per language; `--template overlay`, `--template chat-game` and similar give a working starting point for common plugin types.
