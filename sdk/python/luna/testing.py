"""Run a plugin in-process against a fake Luna: no daemon, capsule or Twitch account.

    from luna.testing import harness
    import main

    def test_welcome():
        t = harness(main)
        t.emit("twitch.channel.follow", user="fox")
        assert t.chat.sent == ["Welcome to the pack, fox!"]

Permissions from the plugin's plugin.toml are enforced, and chat commands are only routed
to users whose rank has the command's capability, so a missing entry fails the test
instead of surprising you after install. Time is fake: t.clock.advance("2m").
"""

import json
import os
import shlex
import tomllib

from . import _bot, errors
from ._bot import Event, Obj, User, bot, seconds

_bot.TESTING = True

# Luna's default capability table: the lowest rank holding each capability.
_MIN_RANK = {
    "chat.commands.use": "viewer", "mod.delete": "moderator", "mod.timeout": "moderator", "mod.warn": "moderator",
    "mod.ban": "moderator", "mod.undo.own": "moderator", "mod.notes": "moderator", "mod.shield": "moderator",
    "stream.shoutout": "moderator", "commands.edit": "moderator", "mod.undo.others": "luna-mod",
    "filters.edit": "luna-mod", "giveaway.run": "luna-mod", "points.adjust": "luna-mod", "stream.edit": "luna-mod",
    "luna.chat_cli": "luna-mod", "mod.lock": "head-mod", "perms.approve_luna_mods": "head-mod",
}
_RANKS = _bot._RANKS


def can(rank, capability):
    """Whether a rank holds a capability under Luna's defaults (no per-user grants)."""
    if rank == "broadcaster":
        return True
    if capability.startswith("mod.") and _RANKS.index(rank) < 1:
        return False
    want = _MIN_RANK.get(capability)
    return want is not None and _RANKS.index(rank) >= _RANKS.index(want)


class Clock:
    def __init__(self):
        self.now = 1767268800.0  # 2026-01-01 12:00 UTC

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


class _Chat:
    def __init__(self):
        self.sent = []      # every message's text
        self.messages = []  # with how it was sent: {"text", "mode", "to", "reply_to"}


class _Mod:
    def __init__(self):
        self.actions = []  # ("timeout", "id-fox", 60), ("delete", "msg-3"), ("shield", True), ...


class _Twitch:
    """Fake Twitch data: t.twitch.users["fox"] = {...}; t.twitch.stream = {"live": True, ...}."""

    def __init__(self):
        self.users = {}     # login → {"id", "login", "name", "created_at"}
        self.channels = {}  # id ("" = this channel) → {"game", "title", ...}
        self.stream = {"live": False}
        self.follows = {}   # user id → "2025-03-01T12:00:00Z"
        self.categories = []  # [{"id", "name"}]
        self.schedule = []    # [{"start", "end", "title", "category"}]
        self.live = {}        # login → {"login", "game", "viewers", ...} for streams()
        self.ads = {"next_ad_at": None, "duration_s": 0, "snooze_count": 0}
        self.writes = []      # ("set_channel", {"title": ...}), ("clip", {}), ("raid", {"to_id": ...})


_TWITCH_PERM = {"user": "read", "follow": "read", "channel": "read", "stream": "read", "categories": "read",
                "schedule": "read", "streams": "read", "set_channel": "edit", "clip": "clips", "marker": "markers",
                "ads": "ads", "snooze": "ads", "raid": "raid", "unraid": "raid"}


class _OSCFake:
    """What the plugin sent to VRChat; set params and avatar to fake what VRChat reports."""

    def __init__(self):
        self.sent = []       # ("/avatar/parameters/Ears", True)
        self.chatbox = []
        self.params = {}
        self.avatar = {"id": "", "name": "", "params": []}


class Result:
    def __init__(self, replies):
        self.replies = replies


_MOD_PERM = {"timeout": "timeout", "ban": "ban", "delete": "delete", "warn": "warn", "shield": "shield",
             "shoutout": "shoutout"}


class _Fake:
    """The transport, faked: records chat and moderation, keeps the store in memory, and
    refuses what plugin.toml doesn't allow, with Luna's own messages."""

    def __init__(self, h):
        self.h = h

    def _need(self, group, item, line):
        p = self.h.perms
        if p is not None and item not in p.get(group, []):
            raise errors.PermissionDenied(f"add {line} to plugin.toml")

    def chat_send(self, text, simulated, reply_to, mode="", to=""):
        need = mode or "send"
        self._need("chat", need, f'"{need}" to [permissions].chat')
        self.h.chat.sent.append(text)
        self.h.chat.messages.append({"text": text, "mode": mode, "to": to, "reply_to": reply_to})

    def mod(self, req):
        action = req["action"]
        need = _MOD_PERM.get(action)
        if action == "unban":
            have = (self.h.perms or {}).get("mod", [])
            need = "timeout" if "timeout" in have or self.h.perms is None else "ban"
        if need is None:
            raise errors.LunaError(f"unknown mod action {action!r}")
        self._need("mod", need, f'"{need}" to [permissions].mod')
        if action == "timeout" and not 1 <= req.get("seconds", 0) <= 1209600:
            raise errors.LunaError("a timeout is 1 s to 14 days")
        detail = {"timeout": lambda: (req["user_id"], req["seconds"]), "delete": lambda: (req["message_id"],),
                  "shield": lambda: (req["on"],)}.get(action, lambda: (req["user_id"],))()
        self.h.mod.actions.append((action, *detail))

    def twitch(self, call, user, args=None, simulated=False):
        need = _TWITCH_PERM.get(call)
        if need is None:
            raise errors.LunaError(f"unknown twitch call {call!r}")
        self._need("twitch", need, f'"{need}" to [permissions].twitch')
        tw = self.h.twitch
        args = args or {}
        v = None
        if need != "read" and call != "ads":
            tw.writes.append((call, args))  # recorded even for simulated events, which harness events are by default
            v = {"clip": {"id": "clip1", "url": "https://clips.twitch.tv/clip1", "edit_url": ""},
                 "marker": {"id": "m1", "position_s": 0}, "snooze": tw.ads}.get(call)
        elif call == "categories":
            v = [c for c in tw.categories if args.get("query", "").lower() in c["name"].lower()]
        elif call == "schedule":
            v = tw.schedule
        elif call == "streams":
            v = [tw.live[l] for l in args.get("logins", []) if l in tw.live]
        elif call == "ads":
            v = tw.ads
        elif call == "user":
            v = next((u for u in tw.users.values()
                      if user.lower() in (str(u.get("login", "")).lower(), str(u.get("id", "")))), None)
        elif call == "channel":
            v = tw.channels.get(user)
        elif call == "stream":
            v = tw.stream
        elif call == "follow":
            v = {"followed_at": tw.follows[user]} if user in tw.follows else None
        return json.dumps(v).encode()

    def osc(self, action, address="", value=b"", type="", text="", name="", simulated=False):
        have = (self.h.perms or {}).get("osc", []) if self.h.perms is not None else None
        if action == "send":
            ok = have is None or any(p == "send:" + address or (p.endswith("*") and ("send:" + address).startswith(p[:-1]))
                                     for p in have)
            if not ok:
                raise errors.PermissionDenied(f'add "send:{address}" to [permissions].osc in plugin.toml')
            self.h.osc.sent.append((address, json.loads(value)))
            return b""
        need = "chatbox" if action == "chatbox" else "read"
        if have is not None and need not in have:
            raise errors.PermissionDenied(f'add "{need}" to [permissions].osc in plugin.toml')
        if action == "chatbox":
            self.h.osc.chatbox.append(text[:144])
            return b""
        if action == "param":
            return json.dumps(self.h.osc.params.get(name)).encode()
        return json.dumps(self.h.osc.avatar).encode()

    def hr(self, minutes):
        if self.h.perms is not None and not _allowed(self.h.perms.get("events", []), "hr.update"):
            raise errors.PermissionDenied('add "hr.update" to [permissions].events in plugin.toml')
        hist = self.h._hr
        if not hist:
            return 0, 0, []
        cut = hist[-1][0] - minutes * 60000
        return hist[-1][1], hist[-1][0], [x for x in hist if minutes > 0 and x[0] >= cut]

    def viewer(self, user):
        p = self.h.perms
        if p is not None and not p.get("viewers"):
            raise errors.PermissionDenied("add viewers = true to [permissions] to plugin.toml")
        v = self.h.viewers.get(user) or next((v for v in self.h.viewers.values() if v.get("login") == user), None)
        return json.dumps(v).encode()

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

    def store_batch(self, sets, deletes):
        s = self._store()
        for k, v, ttl in sets:
            s[k] = (v, self.h.clock.now + ttl / 1000 if ttl else 0)
        for k in deletes:
            s.pop(k, None)

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
        import asyncio
        self.chat, self.mod, self.twitch, self.osc = _Chat(), _Mod(), _Twitch(), _OSCFake()
        self._hr = []  # (at_ms, bpm), from emitted hr.update events
        self.viewers = {}  # user id → profile dict, for bot.viewers.get
        self.store, self.logs, self.emitted = {}, [], []
        self.clock = Clock()
        self._links, self._down, self._cooldowns = {}, set(), {}
        self._n = 0
        self.perms = self._permissions(plugin)
        bot._t = _Fake(self)
        bot.clock = self.clock
        bot.id = "dev.test.plugin"
        bot._timers.clear()
        bot._waiters.clear()
        if bot._loop is not None:
            bot._loop.close()
        bot._loop = asyncio.new_event_loop()
        for h in bot._handlers:
            h.done = False
        for typ in sorted(bot._subscribed()):
            if self.perms is not None and not _allowed(self.perms.get("events", []), typ):
                raise errors.PermissionDenied(f'add "{typ}" to [permissions].events in plugin.toml')
        bot.config = bot._config_cls._load(config or {}) if bot._config_cls else Obj(config or {})
        bot.timers._start()
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

    def _need_store(self):
        if self.perms is not None and not self.perms.get("store"):
            raise errors.PermissionDenied("add store = true to [permissions] in plugin.toml")

    def _id(self, prefix):
        self._n += 1
        return f"{prefix}-{self._n}"

    def emit(self, type, simulated=True, **data):
        """Fire an event at the plugin. user="fox" is shorthand for a viewer named fox."""
        if isinstance(data.get("user"), str):
            data["user"] = _user(data["user"])
        if type == "hr.update" and isinstance(data.get("bpm"), int):
            self._hr = [x for x in self._hr if x[0] >= (self.clock.now - 3600) * 1000] + [(int(self.clock.now * 1000), data["bpm"])]
        ev = Event(self._id("test"), type, "test", self.clock.now, simulated, data)
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
        """Type a chat line as user; returns what the plugin said in response. Roles are Twitch
        badges ("sub", "vip", "moderator", "broadcaster") or Luna ranks ("luna-mod", "head-mod").
        Like Luna, a !command reaches the plugin only if the user's rank has its capability
        and it's off cooldown; every line is also a twitch.chat.message event."""
        before = len(self.chat.sent)
        u = _user(user, roles)
        ev = Event(self._id("test"), "twitch.chat.message", "test", self.clock.now, True,
                   {"user": u, "text": line, "message_id": self._id("msg")})
        bot._dispatch(ev, inline=True)
        words = shlex.split(line) if line.startswith("!") else []
        name = words[0][1:].lower() if words else ""
        cmd = bot._chat_cmds.get(name) or next(
            (c for c in bot._chat_cmds.values() if name in c["aliases"]), None)
        if cmd is None or not can(u["rank"], cmd["capability"] or "chat.commands.use"):
            return Result(self.chat.sent[before:])
        now = self.clock.now
        keys = ((cmd["name"], cmd["cooldown"]), ((cmd["name"], user), cmd["user_cooldown"]))
        if any(cd and self._cooldowns.get(key, -1e18) + seconds(cd) > now for key, cd in keys):
            return Result(self.chat.sent[before:])  # Luna drops commands on cooldown before the plugin sees them
        for key, cd in keys:
            if cd:
                self._cooldowns[key] = now
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
    roles = list(roles)
    rank = next((r for r in ("broadcaster", "head-mod", "luna-mod", "moderator") if r in roles), "viewer")
    if rank in ("head-mod", "luna-mod"):
        roles.append("moderator")
    return {"id": "test-" + login, "login": login, "name": login, "roles": roles, "rank": rank}


def harness(plugin=None, config=None):
    """A fresh fake Luna for the plugin module (already imported)."""
    return Harness(plugin, config)


__all__ = ["harness", "Harness", "Clock", "Result", "can", "User"]
