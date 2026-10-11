"""The `bot` object. Everything a plugin does goes through it, and every call is
checked by Luna against the plugin's permissions."""

import contextlib
import contextvars
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
import typing
from datetime import datetime

from . import errors

API = "1"
SDK = "luna-python/0.2.0"
TESTING = False  # set by luna.testing: bot.run() then returns at once

# The event a handler is answering, so replies to simulated events stay simulated. A
# context variable, so async handlers waiting in wait_for keep theirs.
_current = contextvars.ContextVar("luna_event", default=None)


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

    def __eq__(self, other):
        return isinstance(other, Obj) and self._d == other._d

    def __repr__(self):
        return f"Obj({self._d!r})"


_RANKS = ["viewer", "moderator", "luna-mod", "head-mod", "broadcaster"]


class User(Obj):
    @property
    def name(self):
        return self._d.get("name") or self._d.get("login")

    @property
    def rank(self):
        """Luna's rank for chat messages: viewer, moderator, luna-mod, head-mod or broadcaster."""
        r = self._d.get("rank")
        if r in _RANKS:
            return r
        roles = set(self._d.get("roles") or [])
        if "broadcaster" in roles:
            return "broadcaster"
        return "moderator" if {"moderator", "lead_moderator"} & roles else "viewer"

    def at_least(self, rank):
        return _RANKS.index(self.rank) >= _RANKS.index(rank)

    @property
    def is_mod(self):
        return self.at_least("moderator")


def _time(v):
    """An RFC 3339 time from Luna as a datetime, or None."""
    return datetime.fromisoformat(v.replace("Z", "+00:00")) if isinstance(v, str) and v else None


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
    if k == "text" and isinstance(want, str):
        return str(ev.raw.get("text", "")).strip().lower() == want.lower()
    return ev.raw.get(k) == want


def _uid(user):
    """A Twitch user id from a User, an event's user, or an id string."""
    uid = getattr(user, "id", None) if not isinstance(user, str) else user
    if not uid:
        raise errors.LunaError(f"need a user or a user id, got {user!r}")
    return str(uid)


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


def _check(spec_type, k, v):
    if spec_type == "list":  # a list of tables, like [[plugins.avatar.redeem]]: entries read as objects
        if not isinstance(v, list) or not all(isinstance(t, dict) for t in v):
            raise errors.ConfigError(f"{k}: expected a list of tables, got {v!r}")
        return [Obj(t) for t in v]
    want = {"int": int, "float": (int, float), "str": str, "bool": bool}[spec_type]
    if not isinstance(v, want) or (spec_type == "int" and isinstance(v, bool)):
        raise errors.ConfigError(f"{k}: expected {spec_type}, got {v!r}")
    return float(v) if spec_type == "float" else v


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
            typ = "list" if hint is list or typing.get_origin(hint) is list else _type_name(hint)[0]
            out[k] = {"type": typ, "default": getattr(cls, k)}
        return out

    @classmethod
    def _load(cls, values):
        obj = cls.__new__(cls)
        for k, spec in cls._schema().items():
            object.__setattr__(obj, k, _check(spec["type"], f"config {k}", values.get(k, spec["default"])))
        return obj


# ---------- the API groups ----------

class _Chat:
    def __init__(self, bot):
        self._bot = bot

    def _send(self, text, simulated=None, reply_to="", mode="", to="", wait=False):
        if simulated is None:
            ev = _current.get()
            simulated = bool(ev and ev.simulated)
        for attempt in range(5 if wait else 1):
            try:
                return self._bot._t.chat_send(str(text), simulated, reply_to, mode, to)
            except errors.RateLimited:
                if attempt == 4 or not wait:
                    raise

    def send(self, text, *, simulated=None, wait=False):
        """Send a chat message. Replies to a simulated event go to the terminal, not Twitch.
        wait=True keeps retrying through the rate limit instead of raising RateLimited."""
        self._send(text, simulated, wait=wait)

    def reply(self, text):
        """Answer the chat message being handled, as a threaded reply."""
        ev = _current.get()
        self._send(text, reply_to=(ev.raw.get("message_id", "") if ev else ""))

    def announce(self, text):
        """A highlighted announcement; needs "announce" in [permissions].chat."""
        self._send(text, mode="announce")

    def whisper(self, user, text):
        """A private message; needs "whisper" in [permissions].chat."""
        self._send(text, mode="whisper", to=_uid(user))


class _Mod:
    """Moderation as the channel; each action needs its name in [permissions].mod."""

    def __init__(self, bot):
        self._bot = bot

    def _do(self, action, **req):
        ev = _current.get()
        self._bot._t.mod(dict(action=action, simulated=bool(ev and ev.simulated), **req))

    def timeout(self, user, duration="60s", reason=""):
        self._do("timeout", user_id=_uid(user), seconds=max(1, int(seconds(duration))), reason=reason)

    def ban(self, user, reason=""):
        self._do("ban", user_id=_uid(user), reason=reason)

    def unban(self, user):
        """Lifts a ban or a timeout."""
        self._do("unban", user_id=_uid(user))

    def delete(self, message=None):
        """Deletes a chat message: an event, a message id, or (by default) the one being handled."""
        ev = message if isinstance(message, Event) else _current.get()
        mid = message if isinstance(message, str) else (ev.raw.get("message_id") if ev else None)
        if not mid:
            raise errors.LunaError("delete needs a chat message")
        self._do("delete", message_id=mid)

    def warn(self, user, reason):
        self._do("warn", user_id=_uid(user), reason=reason)

    def shield(self, on=True):
        self._do("shield", on=bool(on))

    def shoutout(self, user):
        """Twitch's native shoutout of a channel (a user or their id)."""
        self._do("shoutout", user_id=_uid(user))


class _Twitch:
    """Public Twitch facts; needs "read" in [permissions].twitch."""

    def __init__(self, bot):
        self._bot = bot

    def _get(self, call, user="", **args):
        ev = _current.get()
        v = json.loads(self._bot._t.twitch(call, user, args, bool(ev and ev.simulated)))
        if isinstance(v, list):
            return [Obj(x) if isinstance(x, dict) else x for x in v]
        return Obj(v) if isinstance(v, dict) else v

    def user(self, login_or_id):
        """id, login, name, created_at — or None if there's no such user."""
        return self._get("user", str(login_or_id).lstrip("@"))

    def channel(self, id=""):
        """game and title of a channel (this one by default)."""
        return self._get("channel", id)

    def stream(self, id=""):
        """live, started_at, game, title, viewers (this channel by default)."""
        return self._get("stream", id)

    def followed_at(self, user):
        """When they followed this channel, or None."""
        f = self._get("follow", _uid(user))
        return _time(f.followed_at) if f else None

    def categories(self, query):
        """Twitch categories matching query, best first: [{id, name}]."""
        return self._get("categories", query=query) or []

    def schedule(self):
        """This channel's upcoming schedule: [{start, end, title, category}]."""
        return self._get("schedule") or []

    def streams(self, logins):
        """Which of these channels are live: [{user_id, login, name, game, title, viewers}]."""
        return self._get("streams", logins=list(logins)) or []

    def set_channel(self, title="", game_id=""):
        """Change the title and/or category (an id from categories). Needs "edit"."""
        self._get("set_channel", title=title, game_id=game_id)

    def clip(self):
        """Clip the last ~30 s of the live stream: {id, url, edit_url}. Needs "clips"."""
        return self._get("clip")

    def marker(self, description=""):
        """Drop a VOD marker: {id, position_s}. Needs "markers"."""
        return self._get("marker", description=description)

    def ads(self):
        """The ad schedule: {next_ad_at, last_ad_at, duration_s, snooze_count, preroll_free_s}. Needs "ads"."""
        return self._get("ads")

    def snooze_ad(self):
        """Push the next ad back 5 minutes. Needs "ads"."""
        return self._get("snooze")

    def raid(self, to_id):
        """Raid the channel with this user ID. Needs "raid"."""
        self._get("raid", to_id=str(to_id))

    def unraid(self):
        """Cancel a pending raid. Needs "raid"."""
        self._get("unraid")


def _simulated():
    ev = _current.get()
    return bool(ev and ev.simulated)


class _OSC:
    """VRChat over OSC; needs [permissions].osc ("send:<address>", "chatbox", "read")."""

    def __init__(self, bot):
        self._bot = bot

    def send(self, address, value):
        """Set an OSC address to a bool, int, float or str. Only the latest value per address
        goes out each 1/30 s."""
        typ = next((n for t, n in ((bool, "bool"), (int, "int"), (float, "float"), (str, "string"))
                    if isinstance(value, t)), None)
        if typ is None:
            raise TypeError(f"OSC can't send a {type(value).__name__}")
        self._bot._t.osc("send", address=address, value=json.dumps(value).encode(), type=typ, simulated=_simulated())

    def set_param(self, name, value):
        """send() to /avatar/parameters/<name>."""
        self.send("/avatar/parameters/" + name, value)

    def chatbox(self, text):
        """Show text in the VRChat chatbox (144 characters; rate-limited, latest wins)."""
        self._bot._t.osc("chatbox", text=str(text), simulated=_simulated())

    def param(self, name):
        """VRChat's last reported value of an avatar parameter, or None."""
        return json.loads(self._bot._t.osc("param", name=name) or b"null")

    def avatar(self):
        """The avatar being worn: id, name, params [{name, type, writable}]."""
        return Obj(json.loads(self._bot._t.osc("avatar") or b"{}"))


class _HR:
    """The streamer's heart rate; needs hr.update in [permissions].events."""

    def __init__(self, bot):
        self._bot = bot

    @property
    def bpm(self):
        """The latest reading; 0 before the first."""
        return self._bot._t.hr(0)[0]

    def history(self, minutes):
        """Readings from the last minutes (up to 60), oldest first: [Obj(at, bpm)]."""
        return [Obj({"at": at / 1000, "bpm": b}) for at, b in self._bot._t.hr(minutes)[2]]


class _Viewers:
    """Luna's shared viewer profile; needs viewers = true."""

    def __init__(self, bot):
        self._bot = bot

    def get(self, user):
        """id, login, first_seen, last_seen, messages, watch_s, streams, streak — or None."""
        v = json.loads(self._bot._t.viewer(user if isinstance(user, str) else _uid(user)))
        return Obj(v) if v else None


class _Model:
    """A typed object saved under one store key on every change: bot.store.model("stats", Stats)."""

    def __init__(self, store, key, cls):
        d = store.get(key, default={}) or {}
        schema = cls._schema()
        vals = {k: _check(s["type"], k, d.get(k, s["default"])) for k, s in schema.items()}
        self.__dict__.update(_store=store, _key=key, _schema=schema, _vals=vals)

    def __getattr__(self, k):
        try:
            return self._vals[k]
        except KeyError:
            raise AttributeError(k) from None

    def __setattr__(self, k, v):
        if k not in self._schema:
            raise AttributeError(f"{k} isn't a field")
        self._vals[k] = _check(self._schema[k]["type"], k, v)
        self._store.set(self._key, self._vals)


class _Store:
    def __init__(self, bot):
        self._bot = bot
        self._tx = contextvars.ContextVar("luna_tx", default=None)

    def get(self, key, default=None):
        v = self._bot._t.store_get(key)
        return default if v is None else json.loads(v)

    def set(self, key, value, ttl=None):
        raw, ttl_ms = json.dumps(value).encode(), int(seconds(ttl) * 1000) if ttl else 0
        tx = self._tx.get()
        if tx is not None:
            tx[0].append((key, raw, ttl_ms))
        else:
            self._bot._t.store_set(key, raw, ttl_ms)

    def delete(self, key):
        tx = self._tx.get()
        if tx is not None:
            tx[1].append(key)
        else:
            self._bot._t.store_delete(key)

    def incr(self, key, by=1):
        """Atomic, so it applies at once even inside a transaction."""
        return self._bot._t.store_incr(key, by)

    def keys(self, prefix=""):
        return self._bot._t.store_keys(prefix)

    @contextlib.contextmanager
    def transaction(self):
        """Groups sets and deletes: all land when the block ends, or none if it raises.
        Reads inside the block see the store as it was before it."""
        if self._tx.get() is not None:
            yield  # nested: part of the outer one
            return
        tx = ([], [])
        token = self._tx.set(tx)
        try:
            yield
        finally:
            self._tx.reset(token)
        if tx[0] or tx[1]:
            self._bot._t.store_batch(tx[0], tx[1])

    def model(self, key, cls):
        """A typed object (fields and defaults like Config) saved under key whenever it changes."""
        return _Model(self, key, cls)


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


_CRON_NAMES = {n: i for i, n in enumerate(["sun", "mon", "tue", "wed", "thu", "fri", "sat"])}
_CRON_NAMES.update({n: i + 1 for i, n in enumerate(
    ["jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"])})


def _cron(spec):
    """Parses "min hour day month weekday" into sets, plus which of day/weekday were restricted."""
    parts = spec.split()
    if len(parts) != 5:
        raise ValueError(f"cron {spec!r}: want 5 fields, minute hour day month weekday")
    out = []
    for f, (lo, hi) in zip(parts, [(0, 59), (0, 23), (1, 31), (1, 12), (0, 7)]):
        vals = set()
        for part in f.lower().split(","):
            rng, _, step = part.partition("/")
            val = lambda s: _CRON_NAMES[s] if s in _CRON_NAMES else int(s)  # noqa: E731
            if rng == "*":
                a, b = lo, hi
            elif "-" in rng:
                a, b = map(val, rng.split("-"))
            else:
                a = b = val(rng)
            if not (lo <= a <= b <= hi):
                raise ValueError(f"cron {spec!r}: {part!r} is out of range {lo}-{hi}")
            vals.update(range(a, b + 1, int(step or 1)))
        out.append(vals)
    if 7 in out[4]:
        out[4].add(0)  # Sunday is 0 or 7
    return out, parts[2] != "*", parts[4] != "*"


def _cron_due(c, t):
    (mins, hours, days, months, wdays), day_set, wday_set = c
    wday = (t.tm_wday + 1) % 7
    day_ok = t.tm_mday in days, wday in wdays
    day = any(day_ok) if day_set and wday_set else all(day_ok)
    return t.tm_min in mins and t.tm_hour in hours and t.tm_mon in months and day


class _Timers:
    """Timers run on the plugin's clock; the test harness's fake clock drives them instantly.
    shortcut: timers live in the capsule, not Luna, so they reset on restart."""

    def __init__(self, bot):
        self._bot = bot
        self._every = []  # (seconds, fn, only_if_chat_active), started by run()
        self._crons = []  # (parsed, fn)

    def after(self, d, fn):
        """Run fn once after d; returns a handle for cancel()."""
        return self._bot._schedule(seconds(d), fn, None)

    def every(self, d, only_if_chat_active=False):
        """@bot.timers.every("15m"); only_if_chat_active skips a run when nobody chatted since the last."""
        def deco(fn):
            self._every.append((seconds(d), fn, only_if_chat_active))
            return fn
        return deco

    def cron(self, spec):
        """@bot.timers.cron("0 20 * * FRI"): minute hour day month weekday, in Luna's local time."""
        parsed = _cron(spec)

        def deco(fn):
            self._crons.append((parsed, fn))
            return fn
        return deco

    def cancel(self, handle):
        if handle:
            handle[2] = None

    def _start(self):
        bot = self._bot
        for interval, fn, active in self._every:
            if active:
                fn = self._when_active(fn)
            bot._schedule(interval, fn, interval)
        if self._crons:
            last = [None]

            def tick():
                t = time.localtime(bot.clock())
                minute = (t.tm_year, t.tm_yday, t.tm_hour, t.tm_min)
                if minute == last[0]:
                    return
                last[0] = minute
                for parsed, fn in self._crons:
                    if _cron_due(parsed, t):
                        bot._call(fn)
            now = bot.clock()
            bot._schedule(60 - now % 60 + 0.5, tick, 60)

    def _when_active(self, fn):
        seen = [self._bot._chat_seen]

        def run():
            if self._bot._chat_seen != seen[0]:
                seen[0] = self._bot._chat_seen
                return fn()
        return run


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
        self._bot.chat._send(text, bool(ev and ev.simulated), ev.raw.get("message_id", "") if ev else "")


class _Handler:
    def __init__(self, typ, fn, when, filters, once, concurrency=1, drop_if_busy=False):
        self.type, self.fn, self.when, self.filters, self.once = typ, fn, when, filters, once
        self.concurrency, self.drop_if_busy = max(1, concurrency), drop_if_busy
        self.done = False
        self.busy = 0
        self.q = None  # per-handler queue in live mode, so each handler sees its events in order
        self.fut = self.loop = None  # wait_for

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
    NotConnected = errors.NotConnected
    Timeout = errors.Timeout

    def __init__(self):
        self.chat = _Chat(self)
        self.mod = _Mod(self)
        self.store = _Store(self)
        self.twitch = _Twitch(self)
        self.viewers = _Viewers(self)
        self.osc = _OSC(self)
        self.hr = _HR(self)
        self.log = _Log(self)
        self.timers = _Timers(self)
        self.links = _Links(self)
        self.lifecycle = _Lifecycle()
        self.crash_on_error = False
        self.config = Obj({})
        self.id = ""
        self._handlers = []
        self._listen = set()
        self._waiters = []
        self._wlock = threading.Lock()
        self._cli = {}
        self._chat_cmds = {}
        self._config_cls = None
        self._t = None  # transport: the gRPC client live, a fake in tests
        self._timers = []  # heap of [due, seq, fn, interval, event]; fn None = cancelled
        self._seq = 0
        self._wake = threading.Condition()
        self._stopping = threading.Event()
        self._chat_seen = 0
        self._loop = None  # asyncio loop for async handlers, made on first use
        self._loop_thread = False  # live: the loop runs in its own thread
        self.clock = time.time

    # --- declaring ---

    def on(self, type, when=None, *, concurrency=1, drop_if_busy=False, **filters):
        """@bot.on("twitch.channel.follow"), @bot.on("twitch.*", when=lambda ev: ...),
        @bot.on("twitch.channel.redeem", reward="Toggle ears"). Each handler gets its events in
        order; concurrency=N runs up to N at once, drop_if_busy skips events while it's busy."""
        def deco(fn):
            self._handlers.append(_Handler(type, fn, when, filters, False, concurrency, drop_if_busy))
            return fn
        return deco

    def once(self, type, when=None, **filters):
        def deco(fn):
            self._handlers.append(_Handler(type, fn, when, filters, True))
            return fn
        return deco

    def listen(self, *types):
        """Have Luna send these events without a handler, for bot.wait_for."""
        self._listen.update(types)

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

    async def wait_for(self, type, timeout=None, when=None, **filters):
        """In an async handler: the next matching event, or None after timeout seconds.
        The type must be one the plugin handles or declared with bot.listen(type)."""
        if not any(_matches(p, type) for p in self._subscribed()):
            raise errors.LunaError(f'bot.wait_for("{type}") needs bot.listen("{type}") at the top of '
                                   "the plugin, so Luna sends it those events")
        import asyncio
        w = _Handler(type, None, when, filters, True)
        w.loop = asyncio.get_running_loop()
        w.fut = w.loop.create_future()
        with self._wlock:
            self._waiters.append(w)
        handle = self._schedule(seconds(timeout), lambda: self._resolve(w, None), None) if timeout else None
        try:
            return await w.fut
        finally:
            with self._wlock:
                if w in self._waiters:
                    self._waiters.remove(w)
            self.timers.cancel(handle)

    async def sleep(self, d):
        """Pause an async handler on the plugin's clock (the fake one in tests)."""
        import asyncio
        loop = asyncio.get_running_loop()
        fut = loop.create_future()
        self._schedule(seconds(d), lambda: loop.call_soon_threadsafe(lambda: fut.done() or fut.set_result(None)), None)
        await fut

    # --- running ---

    def _current(self):
        return _current.get()

    def _resolve(self, w, ev):
        w.loop.call_soon_threadsafe(lambda: w.fut.done() or w.fut.set_result(ev))

    def _subscribed(self):
        types = {h.type for h in self._handlers} | self._listen
        if any(active for _, _, active in self.timers._every):
            types.add("twitch.chat.message")
        return types

    def _call(self, fn, *args, event=None, **kwargs):
        """Run user code with the triggering event in context. Errors are logged, not fatal."""
        token = _current.set(event)
        try:
            res = fn(*args, **kwargs)
            if inspect.iscoroutine(res):
                res = self._run_async(res, event)
            return res
        except Exception as e:
            if self.crash_on_error:
                raise
            self._report(e, event)
            return e
        finally:
            _current.reset(token)

    def _report(self, e, event):
        where = f" handling {event.type} {event.id}" if event else ""
        self._t.log("error", f"{type(e).__name__}{where}: {e}\n{''.join(traceback.format_exception(e, limit=8))}")

    def _ensure_loop(self):
        import asyncio  # only plugins with async handlers pay for asyncio
        with self._wlock:
            if self._loop is None:
                self._loop = asyncio.new_event_loop()
                if not TESTING:
                    self._loop_thread = True
                    threading.Thread(target=self._loop.run_forever, daemon=True).start()
        return self._loop

    def _run_async(self, coro, event):
        import asyncio
        loop = self._ensure_loop()

        async def with_event():
            _current.set(event)
            return await coro
        if self._loop_thread:  # live: this handler's thread waits, so its events stay in order
            return asyncio.run_coroutine_threadsafe(with_event(), loop).result()
        task = loop.create_task(with_event())  # tests: runs until it waits, resumed by later events or time
        self._pump()
        if task.done():
            return task.result()

        def done(t):
            if not t.cancelled() and t.exception():
                self._report(t.exception(), event)
        task.add_done_callback(done)
        return None

    def _pump(self):
        """Tests: let async handlers run until they all wait or finish."""
        if self._loop is None or self._loop_thread:
            return
        for _ in range(20):
            self._loop.call_soon(self._loop.stop)
            self._loop.run_forever()

    def _dispatch(self, ev, inline):
        if ev.type == "twitch.chat.message":
            self._chat_seen += 1
        with self._wlock:
            waiting = [w for w in self._waiters if w.wants(ev)]
            for w in waiting:
                self._waiters.remove(w)
        for w in waiting:
            self._resolve(w, ev)
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
            elif h.drop_if_busy and (h.busy or not h.q.empty()):
                continue
            else:
                try:
                    h.q.put_nowait(ev)
                except queue.Full:
                    self._t.log("warn", f"{h.fn.__name__} is behind; dropped {ev.type} {ev.id}")
        if inline:
            self._pump()

    def _schedule(self, delay, fn, interval):
        """Returns the heap entry, which timers.cancel() takes."""
        with self._wake:
            self._seq += 1
            entry = [self.clock() + delay, self._seq, fn, interval, _current.get()]
            heapq.heappush(self._timers, entry)
            self._wake.notify()
        return entry

    def _run_due(self):
        """Run every timer that's due; returns seconds until the next one (or None)."""
        while True:
            with self._wake:
                if not self._timers:
                    return None
                entry = self._timers[0]
                due, _, fn, interval, ev = entry
                wait = due - self.clock()
                if wait > 0 and fn is not None:
                    return wait
                heapq.heappop(self._timers)
                if fn is None:
                    continue
                if interval:  # re-arm the same entry, so its handle still cancels it
                    self._seq += 1
                    entry[0], entry[1] = due + interval, self._seq
                    heapq.heappush(self._timers, entry)
            self._call(fn, event=ev)
            self._pump()

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
            try:
                for spec, raw in zip(cmd["args"], words):
                    kwargs[spec["name"]] = _convert(spec, raw)
            except ValueError:
                kwargs = None
            missing = [a["name"] for a in cmd["args"][len(words):] if not a.get("optional")]
            if missing or kwargs is None:
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
            "events": sorted(self._subscribed()),
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
