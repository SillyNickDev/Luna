"""Run a plugin in-process against a fake Luna: no daemon, capsule or Twitch account.

    from luna.testing import harness
    import main

    def test_welcome():
        t = harness(main)
        t.emit("twitch.channel.follow", user="fox")
        assert t.chat.sent == ["Welcome to the pack, fox!"]

Permissions from the plugin's plugin.toml are enforced, so a missing entry fails the
test instead of surprising you after install. Time is fake: t.clock.advance("2m").
"""

import json
import os
import shlex
import tomllib

from . import _bot, errors
from ._bot import Event, Obj, bot, seconds

_bot.TESTING = True


class Clock:
    def __init__(self):
        self.now = 1_000_000.0

    def __call__(self):
        return self.now

    def advance(self, d):
        """Move time forward, running every timer that comes due on the way."""
        end = self.now + seconds(d)
        while True:
            wait = bot._run_due()
            if wait is None or self.now + wait > end:
                break
            self.now += wait
        self.now = end
        bot._run_due()


class _Recorder:
    def __init__(self):
        self.sent = []


class Result:
    def __init__(self, replies):
        self.replies = replies


class _Fake:
    """The transport, faked: records chat and logs, keeps the store in memory."""

    def __init__(self, h):
        self.h = h

    def chat_send(self, text, simulated, reply_to):
        self.h._need_chat()
        self.h.chat.sent.append(text)

    def _store(self):
        self.h._need_store()
        now = self.h.clock.now
        for k in [k for k, (_, exp) in self.h.store.items() if exp and exp <= now]:
            del self.h.store[k]
        return self.h.store

    def store_get(self, key):
        v = self._store().get(key)
        return v[0] if v else None

    def store_set(self, key, value, ttl_ms):
        self._store()[key] = (value, self.h.clock.now + ttl_ms / 1000 if ttl_ms else 0)

    def store_delete(self, key):
        self._store().pop(key, None)

    def store_incr(self, key, by):
        s = self._store()
        n = json.loads(s[key][0]) if key in s else 0
        if not isinstance(n, int):
            raise errors.LunaError(f"store: {key!r} doesn't hold an integer")
        s[key] = (json.dumps(n + by).encode(), s[key][1] if key in s else 0)
        return n + by

    def store_keys(self, prefix):
        return sorted(k for k in self._store() if k.startswith(prefix))

    def log(self, level, msg):
        self.h.logs.append((level, msg))

    def emit(self, name, data):
        self.h.emitted.append((name, json.loads(data)))

    def link_call(self, use, method, kwargs):
        provider = use.rsplit(":", 1)[0]
        if provider in self.h._down or provider not in self.h._links:
            raise errors.LinkUnavailable(f"{use}: link_unavailable")
        fn = self.h._links[provider].get(method)
        if fn is None:
            raise errors.LunaError(f"{use}: the fake provider has no {method}")
        return fn(**kwargs)


class Harness:
    def __init__(self, plugin, config):
        self.chat = _Recorder()
        self.store, self.logs, self.emitted = {}, [], []
        self.clock = Clock()
        self._links, self._down, self._cooldowns = {}, set(), {}
        self.perms = self._permissions(plugin)
        bot._t = _Fake(self)
        bot.clock = self.clock
        bot.id = "dev.test.plugin"
        bot._timers.clear()
        for h in bot._handlers:
            h.done = False
            if self.perms is not None and not _allowed(self.perms.get("events", []), h.type):
                raise errors.PermissionDenied(f'add "{h.type}" to [permissions].events in plugin.toml')
        bot.config = bot._config_cls._load(config or {}) if bot._config_cls else Obj(config or {})
        for interval, fn in bot.timers._every:
            bot._schedule(interval, fn, interval)
        for fn in bot.lifecycle.start:
            bot._call(fn)

    @staticmethod
    def _permissions(plugin):
        f = getattr(plugin, "__file__", None)
        path = os.path.join(os.path.dirname(f), "plugin.toml") if f else None
        if not path or not os.path.exists(path):
            return None  # no manifest yet: nothing to enforce
        with open(path, "rb") as fh:
            return tomllib.load(fh).get("permissions", {})

    def _need_chat(self):
        if self.perms is not None and "send" not in self.perms.get("chat", []):
            raise errors.PermissionDenied('add "send" to [permissions].chat in plugin.toml')

    def _need_store(self):
        if self.perms is not None and not self.perms.get("store"):
            raise errors.PermissionDenied("add store = true to [permissions] in plugin.toml")

    def emit(self, type, simulated=True, **data):
        """Fire an event at the plugin. user="fox" is shorthand for a viewer named fox."""
        if isinstance(data.get("user"), str):
            data["user"] = _user(data["user"])
        ev = Event(f"test-{len(self.chat.sent)}", type, "test", self.clock.now, simulated, data)
        bot._dispatch(ev, inline=True)
        return ev

    def cli(self, name, *args, **flags):
        """Run `luna <plugin> name args...`; returns the command's data or text."""
        cmd = bot._cli.get(name)
        if cmd is None:
            raise errors.LunaError(f"no CLI command {name!r}")
        values = {}
        for spec, raw in zip(cmd["args"], args):
            values[spec["name"]] = _bot._convert(spec, raw)
        values.update(flags)
        data, text = bot._command("cli", name, values, None)
        return data if data is not None else text

    def chat_command(self, line, user="testviewer", roles=()):
        """Type a chat line like "!hr" as user; returns what the plugin replied.
        shortcut: capabilities aren't checked here, only cooldowns."""
        words = shlex.split(line)
        name = words[0].lstrip("!")
        cmd = bot._chat_cmds.get(name) or next(
            (c for c in bot._chat_cmds.values() if name in c["aliases"]), None)
        if cmd is None:
            return Result([])
        now = self.clock.now
        for key, cd in ((cmd["name"], cmd["cooldown"]), ((cmd["name"], user), cmd["user_cooldown"])):
            if cd and self._cooldowns.get(key, -1e18) + seconds(cd) > now:
                return Result([])  # Luna drops commands on cooldown before the plugin sees them
        for key, cd in ((cmd["name"], cmd["cooldown"]), ((cmd["name"], user), cmd["user_cooldown"])):
            if cd:
                self._cooldowns[key] = now
        before = len(self.chat.sent)
        ev = Event("test-cmd", "twitch.chat.message", "test", now, True, {"user": _user(user, roles), "text": line})
        bot._command("chat", cmd["name"], {"args": words[1:]}, ev)
        return Result(self.chat.sent[before:])

    def link(self, provider, **fns):
        """Fake a link provider: t.link("dev.sable.overlay", show=lambda **kw: True)."""
        self._links[provider] = fns
        self._down.discard(provider)

    def link_down(self, provider):
        self._down.add(provider)


def _allowed(perms, typ):
    return any(p == typ or (p.endswith(".*") and typ.startswith(p[:-1])) for p in perms)


def _user(login, roles=()):
    return {"id": "test-" + login, "login": login, "roles": list(roles)}


def harness(plugin=None, config=None):
    """A fresh fake Luna for the plugin module (already imported)."""
    return Harness(plugin, config)
