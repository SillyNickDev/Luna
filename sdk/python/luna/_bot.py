"""The `bot` object. Everything a plugin does goes through it, and every call is
checked by Luna against the plugin's permissions."""

import heapq
import inspect
import json
import os
import queue
import re
import sys
import threading
import time
import traceback
import types
import typing

from . import errors

API = "1"
SDK = "luna-python/0.1.0"
TESTING = False  # set by luna.testing: bot.run() then returns at once


# ---------- small helpers ----------

_DUR = re.compile(r"^(\d+(?:\.\d+)?)(ms|s|m|h|d)?$")


def seconds(d):
    """'10s', '2m', '1h', '500ms' or a number of seconds."""
    if isinstance(d, (int, float)):
        return float(d)
    m = _DUR.match(str(d).strip())
    if not m:
        raise ValueError(f"bad duration {d!r} (want e.g. 10s, 2m, 1h)")
    n, unit = float(m.group(1)), m.group(2) or "s"
    return n * {"ms": 0.001, "s": 1, "m": 60, "h": 3600, "d": 86400}[unit]


class Obj:
    """Read-only attribute view of a JSON object: ev.user.login. Missing fields are None."""

    def __init__(self, d):
        self.__dict__["_d"] = d or {}

    def __getattr__(self, k):
        v = self._d.get(k)
        return Obj(v) if isinstance(v, dict) else v

    def __getitem__(self, k):
        return self._d[k]

    def get(self, k, default=None):
        return self._d.get(k, default)

    def __setattr__(self, k, v):
        raise AttributeError("events are read-only")

    def __repr__(self):
        return f"Obj({self._d!r})"


_RANKS = ["viewer", "moderator", "luna-mod", "head-mod", "broadcaster"]


class User(Obj):
    @property
    def name(self):
        return self._d.get("name") or self._d.get("login")

    @property
    def is_mod(self):
        rank = self._d.get("rank")
        if rank in _RANKS:
            return _RANKS.index(rank) >= 1
        return bool({"moderator", "lead_moderator", "broadcaster"} & set(self._d.get("roles") or []))


class Event:
    """A bus event. Payload fields are attributes (ev.user.name, ev.bpm); ev.raw is the whole payload."""

    def __init__(self, id, type, source, ts, simulated, data):
        self.id, self.type, self.source, self.ts, self.simulated = id, type, source, ts, simulated
        self.raw = data if isinstance(data, dict) else {}

    def __getattr__(self, k):
        if k == "user":
            u = self.raw.get("user")
            return User(u) if isinstance(u, dict) else None
        v = self.raw.get(k)
        return Obj(v) if isinstance(v, dict) else v

    def __repr__(self):
        return f"<Event {self.type} {self.raw!r}>"


def _matches(pattern, typ):
    return typ.startswith(pattern[:-1]) if pattern.endswith("*") else pattern == typ


def _field_ok(ev, k, want):
    if k == "user":
        u = ev.raw.get("user") or {}
        return str(want).lower() in (str(u.get("login", "")).lower(), str(u.get("id", "")))
    return ev.raw.get(k) == want


# ---------- config ----------

_TYPES = {int: "int", float: "float", str: "str", bool: "bool"}


def _type_name(hint):
    """int, float, str or bool for a hint, unwrapping Optional[...]/X | None."""
    args = typing.get_args(hint)
    if args and type(None) in args:
        hint = next(a for a in args if a is not type(None))
    if hint not in _TYPES:
        raise TypeError(f"type {hint!r} isn't supported; use int, float, str or bool")
    return _TYPES[hint], bool(args and type(None) in args)


class Config:
    """Subclass with typed fields and defaults; bot.use_config(Settings) loads it from Luna."""

    @classmethod
    def _schema(cls):
        out = {}
        for k, hint in typing.get_type_hints(cls).items():
            if k.startswith("_"):
                continue
            if not hasattr(cls, k):
                raise errors.ConfigError(f"config field {k} needs a default")
            out[k] = {"type": _type_name(hint)[0], "default": getattr(cls, k)}
        return out

    @classmethod
    def _load(cls, values):
        obj = cls.__new__(cls)
        for k, spec in cls._schema().items():
            v = values.get(k, spec["default"])
            want = {"int": int, "float": (int, float), "str": str, "bool": bool}[spec["type"]]
            if not isinstance(v, want) or (spec["type"] == "int" and isinstance(v, bool)):
                raise errors.ConfigError(f"config {k}: expected {spec['type']}, got {v!r}")
            object.__setattr__(obj, k, float(v) if spec["type"] == "float" else v)
        return obj


# ---------- the API groups ----------

class _Chat:
    def __init__(self, bot):
        self._bot = bot

    def send(self, text, *, simulated=None):
        """Send a chat message. Replies to a simulated event go to the terminal, not Twitch."""
        ev = self._bot._current()
        if simulated is None:
            simulated = bool(ev and ev.simulated)
        self._bot._t.chat_send(str(text), simulated, "")


class _Store:
    def __init__(self, bot):
        self._bot = bot

    def get(self, key, default=None):
        v = self._bot._t.store_get(key)
        return default if v is None else json.loads(v)

    def set(self, key, value, ttl=None):
        self._bot._t.store_set(key, json.dumps(value).encode(), int(seconds(ttl) * 1000) if ttl else 0)

    def delete(self, key):
        self._bot._t.store_delete(key)

    def incr(self, key, by=1):
        return self._bot._t.store_incr(key, by)

    def keys(self, prefix=""):
        return self._bot._t.store_keys(prefix)


class _Log:
    def __init__(self, bot):
        self._bot = bot

    def _log(self, level, msg):
        self._bot._t.log(level, str(msg))

    def debug(self, msg):
        self._log("debug", msg)

    def info(self, msg):
        self._log("info", msg)

    def warn(self, msg):
        self._log("warn", msg)

    def error(self, msg):
        self._log("error", msg)


class _Timers:
    """Timers run on the plugin's clock; the test harness's fake clock drives them instantly.
    shortcut: timers live in the capsule, not Luna, so they reset on restart; cron isn't supported yet."""

    def __init__(self, bot):
        self._bot = bot
        self._every = []  # (seconds, fn), started by run()

    def after(self, d, fn):
        self._bot._schedule(seconds(d), fn, None)

    def every(self, d):
        def deco(fn):
            self._every.append((seconds(d), fn))
            return fn
        return deco


class _LinkClient:
    def __init__(self, bot, provider, names):
        self._bot, self._provider, self._names = bot, provider, names

    def __getattr__(self, name):
        if name.startswith("_"):
            raise AttributeError(name)
        if name not in self._names:
            raise errors.PermissionDenied(
                f'declare the link first: bot.links.use("{self._provider}", "{name}") '
                f'and add "{self._provider}:{name}" to [links].uses')

        def call(**kwargs):
            return self._bot._t.link_call(f"{self._provider}:{name}", name, kwargs)
        return call


class _Links:
    def __init__(self, bot):
        self._bot = bot
        self._provided = {}
        self._uses = []

    def provide(self, name):
        def deco(fn):
            self._provided[name] = fn
            return fn
        return deco

    def use(self, provider, *names):
        """overlay = bot.links.use("dev.sable.overlay", "show"); overlay.show(text="hi")"""
        self._uses += [f"{provider}:{n}" for n in names]
        return _LinkClient(self._bot, provider, names)


class _Lifecycle:
    def __init__(self):
        self.start, self.stop = [], []

    def on_start(self, fn):
        self.start.append(fn)
        return fn

    def on_stop(self, fn):
        self.stop.append(fn)
        return fn


class Ctx:
    """What a chat command handler gets: ctx.reply, ctx.user, ctx.event."""

    def __init__(self, bot, event):
        self._bot, self.event = bot, event
        self.user = event.user if event else None

    def reply(self, text):
        ev = self.event
        self._bot._t.chat_send(str(text), bool(ev and ev.simulated), ev.id if ev else "")


class _Handler:
    def __init__(self, typ, fn, when, filters, once):
        self.type, self.fn, self.when, self.filters, self.once = typ, fn, when, filters, once
        self.done = False
        self.q = None  # per-handler queue in live mode, so each handler sees its events in order

    def wants(self, ev):
        if self.done or not _matches(self.type, ev.type):
            return False
        if not all(_field_ok(ev, k, v) for k, v in self.filters.items()):
            return False
        return self.when is None or bool(self.when(ev))


def _arg_spec(fn, skip_ctx):
    """Args and flags from a function signature: bool parameters are flags, the rest args."""
    args, flags = [], []
    hints = typing.get_type_hints(fn)
    params = list(inspect.signature(fn).parameters.values())
    if skip_ctx and params:
        params = params[1:]
    for p in params:
        typ, optional = _type_name(hints.get(p.name, str))
        entry = {"name": p.name, "type": typ}
        if p.default is not inspect.Parameter.empty or optional:
            entry["optional"] = True
        (flags if typ == "bool" else args).append(entry)
    return args, flags


def _convert(spec, raw):
    conv = {"int": int, "float": float, "str": str, "bool": lambda v: v in (True, "true", "1", "yes")}
    return conv[spec["type"]](raw)


class Bot:
    LunaError = errors.LunaError
    PermissionDenied = errors.PermissionDenied
    RateLimited = errors.RateLimited
    LinkUnavailable = errors.LinkUnavailable
    ConfigError = errors.ConfigError
    Timeout = errors.Timeout

    def __init__(self):
        self.chat = _Chat(self)
        self.store = _Store(self)
        self.log = _Log(self)
        self.timers = _Timers(self)
        self.links = _Links(self)
        self.lifecycle = _Lifecycle()
        self.crash_on_error = False
        self.config = Obj({})
        self.id = ""
        self._handlers = []
        self._cli = {}
        self._chat_cmds = {}
        self._config_cls = None
        self._t = None  # transport: the gRPC client live, a fake in tests
        self._local = threading.local()
        self._timers = []  # heap of (due, seq, fn, interval)
        self._seq = 0
        self._wake = threading.Condition()
        self._stopping = threading.Event()
        self.clock = time.monotonic

    # --- declaring ---

    def on(self, type, when=None, **filters):
        """@bot.on("twitch.channel.follow") or @bot.on("twitch.*", when=lambda ev: ...)"""
        def deco(fn):
            self._handlers.append(_Handler(type, fn, when, filters, False))
            return fn
        return deco

    def once(self, type, when=None, **filters):
        def deco(fn):
            self._handlers.append(_Handler(type, fn, when, filters, True))
            return fn
        return deco

    def cli(self, name, mutates=False):
        """@bot.cli("threshold"): becomes `luna <plugin> threshold`, args from the signature."""
        def deco(fn):
            args, flags = _arg_spec(fn, skip_ctx=False)
            self._cli[name] = {"fn": fn, "name": name, "summary": (inspect.getdoc(fn) or "").split("\n")[0],
                               "args": args, "flags": flags, "mutates": mutates}
            return fn
        return deco

    def chat_command(self, name, aliases=(), cooldown=None, user_cooldown=None, capability=None):
        """@bot.chat_command("hr", cooldown="10s"): Luna checks rank and cooldowns before calling it."""
        def deco(fn):
            args, _ = _arg_spec(fn, skip_ctx=True)
            self._chat_cmds[name] = {"fn": fn, "name": name, "aliases": list(aliases), "args": args,
                                     "cooldown": cooldown, "user_cooldown": user_cooldown,
                                     "capability": capability}
            return fn
        return deco

    def use_config(self, cls):
        self._config_cls = cls
        self.config = cls._load({})

    def emit(self, name, **data):
        self._t.emit(name, json.dumps(data).encode())

    # --- running ---

    def _current(self):
        return getattr(self._local, "event", None)

    def _call(self, fn, *args, event=None, **kwargs):
        """Run user code with the triggering event in context. Errors are logged, not fatal."""
        prev = self._current()
        self._local.event = event
        try:
            res = fn(*args, **kwargs)
            if inspect.iscoroutine(res):
                import asyncio  # only plugins with async handlers pay for asyncio
                res = asyncio.run(res)
            return res
        except Exception as e:
            if self.crash_on_error:
                raise
            where = f" handling {event.type} {event.id}" if event else ""
            self._t.log("error", f"{type(e).__name__}{where}: {e}\n{traceback.format_exc(limit=8)}")
            return e
        finally:
            self._local.event = prev

    def _dispatch(self, ev, inline):
        for h in self._handlers:
            try:
                wanted = h.wants(ev)
            except Exception as e:
                self._t.log("error", f"`when` filter for {h.fn.__name__} raised {e!r}")
                continue
            if not wanted:
                continue
            if h.once:
                h.done = True
            if inline:
                self._call(h.fn, ev, event=ev)
            else:
                try:
                    h.q.put_nowait(ev)
                except queue.Full:
                    self._t.log("warn", f"{h.fn.__name__} is behind; dropped {ev.type} {ev.id}")

    def _schedule(self, delay, fn, interval):
        with self._wake:
            self._seq += 1
            heapq.heappush(self._timers, (self.clock() + delay, self._seq, fn, interval))
            self._wake.notify()

    def _run_due(self):
        """Run every timer that's due; returns seconds until the next one (or None)."""
        while True:
            with self._wake:
                if not self._timers:
                    return None
                due, _, fn, interval = self._timers[0]
                wait = due - self.clock()
                if wait > 0:
                    return wait
                heapq.heappop(self._timers)
                if interval:
                    self._seq += 1
                    heapq.heappush(self._timers, (due + interval, self._seq, fn, interval))
            self._call(fn)

    def _command(self, kind, name, args, event):
        """Run a CLI or chat command; returns (data, text)."""
        if kind == "cli":
            cmd = self._cli.get(name)
            if not cmd:
                raise errors.LunaError(f"no CLI command {name!r}")
            res = self._call(cmd["fn"], **args)
        else:
            cmd = self._chat_cmds.get(name)
            if not cmd:
                raise errors.LunaError(f"no chat command {name!r}")
            words = args.get("args", [])
            kwargs = {}
            for spec, raw in zip(cmd["args"], words):
                kwargs[spec["name"]] = _convert(spec, raw)
            missing = [a["name"] for a in cmd["args"][len(words):] if not a.get("optional")]
            if missing:
                Ctx(self, event).reply(f"usage: !{name} " + " ".join(f"<{a['name']}>" for a in cmd["args"]))
                return None, ""
            res = self._call(cmd["fn"], Ctx(self, event), event=event, **kwargs)
        if isinstance(res, Exception):
            raise res
        if res is None or isinstance(res, str):
            return None, res or ""
        return res, ""

    def describe(self):
        """What luna capsule dev writes into plugin.toml."""
        return {
            "events": sorted({h.type for h in self._handlers}),
            "cli": [{k: v for k, v in c.items() if k != "fn"} for c in self._cli.values()],
            "chat": [{"name": c["name"], "aliases": c["aliases"], "capability": c["capability"],
                      "cooldown": c["cooldown"], "user_cooldown": c["user_cooldown"]}
                     for c in self._chat_cmds.values()],
            "config": self._config_cls._schema() if self._config_cls else {},
            "links": {"provides": sorted(self.links._provided), "uses": sorted(set(self.links._uses))},
        }

    def run(self):
        """Connect to Luna and run until it says stop. Under the test harness this returns at once."""
        if TESTING:
            return
        if os.environ.get("LUNA_DESCRIBE"):
            json.dump(self.describe(), sys.stdout)
            sys.stdout.write("\n")
            return
        if not os.environ.get("CAPSULE_CONTROL"):
            sys.exit("this is a Luna plugin; run it with: luna capsule dev")
        from ._live import run
        run(self)


bot = Bot()
