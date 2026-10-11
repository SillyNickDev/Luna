"""Running inside a capsule: the control channel to the runtime and the plugin API to Luna."""

import json
import os
import queue
import socket
import struct
import sys
import threading

import grpc

from . import _cbor, errors
from ._bot import API, SDK, Event, Obj
from ._pb import plugin_pb2 as pb
from ._pb import plugin_pb2_grpc as pbg

MAX_FRAME = 1 << 20


class Frames:
    """Length-prefixed CBOR frames over a Unix socket (Capsules Spec, Framing)."""

    def __init__(self, path):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(path)
        self.wlock = threading.Lock()

    def send(self, t, id=0, body=None):
        f = {"t": t}
        if id:
            f["id"] = id
        if body is not None:
            f["b"] = body
        b = _cbor.dumps(f)
        with self.wlock:
            self.sock.sendall(struct.pack(">I", len(b)) + b)

    def recv(self):
        n = struct.unpack(">I", self._read(4))[0]
        if n > MAX_FRAME:
            raise ConnectionError("frame over 1 MiB")
        return _cbor.loads(self._read(n))

    def _read(self, n):
        buf = b""
        while len(buf) < n:
            chunk = self.sock.recv(n - len(buf))
            if not chunk:
                raise ConnectionError("closed")
            buf += chunk
        return buf


def _err(e):
    """A gRPC error as the SDK error the spec names."""
    code, msg = e.code(), e.details()
    if code == grpc.StatusCode.PERMISSION_DENIED:
        return errors.PermissionDenied(msg)
    if code == grpc.StatusCode.RESOURCE_EXHAUSTED:
        return errors.RateLimited(msg)
    if code == grpc.StatusCode.UNAVAILABLE:
        return errors.NotConnected("Luna is unreachable: " + msg)
    if code == grpc.StatusCode.DEADLINE_EXCEEDED:
        return errors.Timeout(msg)
    return errors.LunaError(msg)


class Transport:
    """The real thing: gRPC to Luna on $CAPSULE_HOST, link calls through the broker."""

    def __init__(self):
        self.ch = grpc.insecure_channel("unix:" + os.environ["CAPSULE_HOST"],
                                        options=[("grpc.max_receive_message_length", 1 << 20)])
        self.stub = pbg.HostStub(self.ch)
        self.links = {}
        self.links_lock = threading.Lock()
        self.link_seq = 0

    def _rpc(self, fn, req):
        try:
            return fn(req, timeout=35)
        except grpc.RpcError as e:
            raise _err(e) from None

    def hello(self):
        return self._rpc(self.stub.Hello, pb.HelloRequest(api=API, sdk=SDK))

    def chat_send(self, text, simulated, reply_to, mode="", to=""):
        self._rpc(self.stub.ChatSend, pb.ChatSendRequest(text=text, simulated=simulated, reply_to=reply_to,
                                                         mode=mode, to_user_id=to))

    def mod(self, req):
        self._rpc(self.stub.Mod, pb.ModRequest(**req))

    def twitch(self, call, user, args=None, simulated=False):
        return self._rpc(self.stub.Twitch, pb.TwitchRequest(call=call, user=user, simulated=simulated,
                                                            args_json=json.dumps(args).encode() if args else b"")).data_json

    def osc(self, action, address="", value=b"", type="", text="", name="", simulated=False):
        return self._rpc(self.stub.Osc, pb.OscRequest(action=action, address=address, value_json=value, type=type,
                                                      text=text, name=name, simulated=simulated)).data_json

    def hr(self, minutes):
        r = self._rpc(self.stub.Hr, pb.HrRequest(minutes=minutes))
        return r.bpm, r.at_ms, [(s.at_ms, s.bpm) for s in r.history]

    def viewer(self, user):
        return self._rpc(self.stub.Viewer, pb.ViewerRequest(user=user)).data_json

    def store_batch(self, sets, deletes):
        self._rpc(self.stub.StoreBatch, pb.StoreBatchRequest(
            set=[pb.StoreSetRequest(key=k, value_json=v, ttl_ms=t) for k, v, t in sets], delete=deletes))

    def store_get(self, key):
        r = self._rpc(self.stub.StoreGet, pb.StoreKey(key=key))
        return r.value_json if r.found else None

    def store_set(self, key, value, ttl_ms):
        self._rpc(self.stub.StoreSet, pb.StoreSetRequest(key=key, value_json=value, ttl_ms=ttl_ms))

    def store_delete(self, key):
        self._rpc(self.stub.StoreDelete, pb.StoreKey(key=key))

    def store_incr(self, key, by):
        return self._rpc(self.stub.StoreIncr, pb.StoreIncrRequest(key=key, by=by)).value

    def store_keys(self, prefix):
        return list(self._rpc(self.stub.StoreKeys, pb.StoreKeysRequest(prefix=prefix)).keys)

    def log(self, level, msg):
        try:
            self.stub.Log(pb.LogRequest(level=level, msg=msg), timeout=5)
        except grpc.RpcError:
            print(f"[{level}] {msg}", file=sys.stderr, flush=True)

    def emit(self, name, data):
        self._rpc(self.stub.Emit, pb.EmitRequest(name=name, data_json=data))

    def link_call(self, use, method, kwargs):
        with self.links_lock:
            conn = self.links.get(use)
            if conn is None:
                try:
                    conn = Frames(os.path.join(os.environ["CAPSULE_LINKS"], use))
                except OSError:
                    raise errors.LinkUnavailable(f"{use}: not approved or not declared in [links].uses") from None
                self.links[use] = conn
            self.link_seq += 1
            seq = self.link_seq
            try:
                conn.send("call", seq, {"method": method, "body": kwargs})
                conn.sock.settimeout(10)
                f = conn.recv()
            except OSError:
                self.links.pop(use, None)
                raise errors.LinkUnavailable(f"{use}: the link dropped") from None
        r = f.get("b") or {}
        if not r.get("ok"):
            err = r.get("error", "failed")
            if err in ("link_unavailable", "timeout"):
                raise errors.LinkUnavailable(f"{use}: {err}")
            raise errors.LunaError(f"{use}: {err}")
        return r.get("body")


def _event(e):
    return Event(e.id, e.type, e.source, e.ts_ms / 1000, e.simulated, json.loads(e.data_json or b"{}"))


def run(bot):
    ctl = Frames(os.environ["CAPSULE_CONTROL"])
    ctl.send("hello", body={"spec": "0.1", "pid": os.getpid()})
    if ctl.recv().get("t") != "welcome":
        sys.exit("the runtime didn't welcome this capsule")

    t = Transport()
    bot._t = t
    try:
        hello = t.hello()
    except errors.LunaError as e:
        sys.exit(f"Luna refused the plugin API: {e}")
    bot.id = hello.id
    cfg = json.loads(hello.config_json or b"{}")
    bot.config = bot._config_cls._load(cfg) if bot._config_cls else Obj(cfg)

    def fatal(msg):
        print(msg, file=sys.stderr, flush=True)
        os._exit(1)

    # Each handler gets its own ordered queue and thread (or `concurrency` threads), so a
    # slow one doesn't hold up the others.
    for h in bot._handlers:
        h.q = queue.Queue(1000)

        def work(h=h):
            while True:
                ev = h.q.get()
                h.busy += 1
                try:
                    bot._call(h.fn, ev, event=ev)
                finally:
                    h.busy -= 1
        for _ in range(h.concurrency):
            threading.Thread(target=work, daemon=True).start()

    types = bot.describe()["events"]
    if types:
        def events():
            try:
                for e in t.stub.Subscribe(pb.SubscribeRequest(types=types)):
                    bot._dispatch(_event(e), inline=False)
            except grpc.RpcError as e:
                if not bot._stopping.is_set():
                    fatal(f"event stream ended: {_err(e)}")
        threading.Thread(target=events, daemon=True).start()

    if bot._cli or bot._chat_cmds:
        results = queue.Queue()

        def outgoing():
            while True:
                yield results.get()

        def commands():
            try:
                for c in t.stub.Commands(outgoing()):
                    threading.Thread(target=one_command, args=(c,), daemon=True).start()
            except grpc.RpcError as e:
                if not bot._stopping.is_set():
                    fatal(f"command stream ended: {_err(e)}")

        def one_command(c):
            r = pb.CommandResult(id=c.id)
            try:
                ev = _event(c.event) if c.HasField("event") else None
                data, text = bot._command(c.kind, c.name, json.loads(c.args_json or b"{}"), ev)
                r.text = text
                if data is not None:
                    r.data_json = json.dumps(data).encode()
            except Exception as e:
                r.error = str(e) or type(e).__name__
            results.put(r)
        threading.Thread(target=commands, daemon=True).start()

    bot.timers._start()

    def timers():
        while not bot._stopping.is_set():
            wait = bot._run_due()
            with bot._wake:
                bot._wake.wait(wait)
    threading.Thread(target=timers, daemon=True).start()

    def provide(f):
        b = f.get("b") or {}
        fn = bot.links._provided.get(b.get("link"))
        res = {"ok": False, "error": "no such link"}
        if fn:
            out = bot._call(fn, **(b.get("body") or {}))
            res = {"ok": False, "error": str(out)} if isinstance(out, Exception) else {"ok": True, "body": out}
        ctl.send("result", f.get("id", 0), res)

    for fn in bot.lifecycle.start:
        bot._call(fn)
    ctl.send("ready")
    while True:
        try:
            f = ctl.recv()
        except (ConnectionError, OSError):
            break
        kind = f.get("t")
        if kind == "ping":
            ctl.send("pong", f.get("id", 0))
        elif kind == "call":
            threading.Thread(target=provide, args=(f,), daemon=True).start()
        elif kind == "shutdown":
            break
    bot._stopping.set()
    for fn in bot.lifecycle.stop:
        bot._call(fn)
    os._exit(0)
