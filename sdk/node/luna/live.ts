// The capsule side of a live plugin: the runtime's control channel (Capsules spec) and
// the plugin API over gRPC.
import https from "node:https";
import net from "node:net";
import tls from "node:tls";
import path from "node:path";
import { decode as cborDecode, encode as cborEncode } from "./cbor.ts";
import { Client, Code, GrpcError } from "./grpc.ts";
import {
  type Bot, type Json, type Transport, LinkUnavailable, LunaError, makeEvent, NotConnected, PermissionDenied, RateLimited, Timeout,
} from "./bot.ts";

const API = "1", SDK = "luna-node/0.1.0";
const enc = new TextEncoder(), dec = new TextDecoder();
const json = (v: Json) => enc.encode(JSON.stringify(v));
const parse = (b: Uint8Array) => (b.length ? JSON.parse(dec.decode(b)) : null);

function wrap(e: unknown): Error {
  if (!(e instanceof GrpcError)) return e as Error;
  switch (e.code) {
    case Code.PermissionDenied: return new PermissionDenied(e.message);
    case Code.ResourceExhausted: return new RateLimited(e.message);
    case Code.Unavailable: return new NotConnected(e.message);
    case Code.DeadlineExceeded: return new Timeout(e.message);
  }
  return new LunaError(e.message);
}

/** Frames: a 4-byte big-endian length, then a CBOR map {t, id, b}. */
export class Frames {
  sock: net.Socket;
  private buf = Buffer.alloc(0);
  private waiting: ((f: any) => void)[] = [];
  private queued: any[] = [];
  private closed = false;

  constructor(sock: net.Socket) {
    this.sock = sock;
    sock.on("data", (c: Buffer) => {
      this.buf = Buffer.concat([this.buf, c]);
      while (this.buf.length >= 4) {
        const n = this.buf.readUInt32BE(0);
        if (n > 1 << 20) {
          sock.destroy();
          return;
        }
        if (this.buf.length < 4 + n) break;
        const f = cborDecode(this.buf.subarray(4, 4 + n));
        this.buf = this.buf.subarray(4 + n);
        const w = this.waiting.shift();
        if (w) w(f);
        else this.queued.push(f);
      }
    });
    const end = () => {
      this.closed = true;
      for (const w of this.waiting.splice(0)) w(null);
    };
    sock.on("close", end);
    sock.on("error", end);
  }

  static connect(p: string): Promise<Frames> {
    return new Promise((resolve, reject) => {
      const s = net.connect(p, () => resolve(new Frames(s)));
      s.once("error", reject);
    });
  }

  send(t: string, id = 0, body?: Json): void {
    const f: Json = { t };
    if (id) f.id = id;
    if (body !== undefined) f.b = body;
    const b = cborEncode(f);
    const head = Buffer.alloc(4);
    head.writeUInt32BE(b.length);
    this.sock.write(Buffer.concat([head, b]));
  }

  /** The next frame, or null once the connection is gone. */
  recv(): Promise<any> {
    if (this.queued.length) return Promise.resolve(this.queued.shift());
    if (this.closed) return Promise.resolve(null);
    return new Promise((r) => this.waiting.push(r));
  }
}

class Live implements Transport {
  c: Client;
  private links = new Map<string, Promise<Frames>>();
  private seq = 0;
  private linkQueue = new Map<string, Promise<unknown>>();

  constructor(sock: string) {
    this.c = new Client(sock);
  }

  async call(method: string, req: string, res: string, m: Json): Promise<Json> {
    try {
      return await this.c.unary(method, req, res, m);
    } catch (e) {
      throw wrap(e);
    }
  }

  async chatSend(text: string, simulated: boolean, replyTo: string, mode: string, to: string) {
    await this.call("ChatSend", "ChatSendRequest", "Empty", { text, simulated, reply_to: replyTo, mode, to_user_id: to });
  }
  async mod(req: Json) { await this.call("Mod", "ModRequest", "Empty", req); }
  async twitch(call: string, user: string, args: Json, simulated: boolean) {
    const r = await this.call("Twitch", "TwitchRequest", "TwitchReply",
      { call, user, args_json: args === undefined ? undefined : json(args), simulated });
    return parse(r.data_json);
  }
  async viewer(user: string) { return parse((await this.call("Viewer", "ViewerRequest", "ViewerReply", { user })).data_json); }
  async storeGet(key: string) {
    const r = await this.call("StoreGet", "StoreKey", "StoreValue", { key });
    return r.found ? parse(r.value_json) : undefined;
  }
  async storeSet(key: string, value: Json, ttlMs: number) {
    await this.call("StoreSet", "StoreSetRequest", "Empty", { key, value_json: json(value), ttl_ms: ttlMs });
  }
  async storeDelete(key: string) { await this.call("StoreDelete", "StoreKey", "Empty", { key }); }
  async storeIncr(key: string, by: number) { return (await this.call("StoreIncr", "StoreIncrRequest", "StoreIncrReply", { key, by })).value; }
  async storeKeys(prefix: string) { return (await this.call("StoreKeys", "StoreKeysRequest", "StoreKeysReply", { prefix })).keys; }
  async storeBatch(sets: [string, Json, number][], deletes: string[]) {
    await this.call("StoreBatch", "StoreBatchRequest", "Empty",
      { set: sets.map(([key, v, ttl]) => ({ key, value_json: json(v), ttl_ms: ttl })), delete: deletes });
  }
  async log(level: string, msg: string) {
    try {
      await this.c.unary("Log", "LogRequest", "Empty", { level, msg }, 5000);
    } catch {
      console.error(`[${level}] ${msg}`);
    }
  }
  async emit(name: string, data: Json) { await this.call("Emit", "EmitRequest", "Empty", { name, data_json: json(data) }); }
  async osc(req: Json) {
    const r = await this.call("Osc", "OscRequest", "OscReply",
      { ...req, value: undefined, value_json: req.value === undefined ? undefined : json(req.value) });
    return parse(r.data_json);
  }
  async hr(minutes: number) {
    const r = await this.call("Hr", "HrRequest", "HrReply", { minutes });
    return { bpm: r.bpm, atMs: r.at_ms, history: r.history.map((s: Json) => ({ atMs: s.at_ms, bpm: s.bpm })) };
  }

  // HTTPS through the egress proxy: CONNECT on the capsule's socket, then TLS to the host.
  async http(method: string, url: string, headers: Record<string, string>, body: Uint8Array | undefined, timeoutMs: number) {
    const u = new URL(url);
    if (u.protocol !== "https:") throw new LunaError("bot.http is HTTPS only");
    const sock = process.env.CAPSULE_EGRESS;
    if (!sock) throw new PermissionDenied('no network: list the hosts this plugin needs in [capsule].net, like "api.example.com:443"');
    const target = `${u.hostname}:${u.port || 443}`;
    const raw = await new Promise<net.Socket>((resolve, reject) => {
      const s = net.connect(sock);
      let head = Buffer.alloc(0);
      s.once("error", reject);
      s.once("connect", () => s.write(`CONNECT ${target} HTTP/1.1\r\nHost: ${target}\r\n\r\n`));
      const onData = (c: Buffer) => {
        head = Buffer.concat([head, c]);
        const end = head.indexOf("\r\n\r\n");
        if (end < 0) return;
        s.off("data", onData);
        const status = head.subarray(0, head.indexOf("\r\n")).toString().split(" ")[1];
        if (status !== "200") {
          s.destroy();
          const why = head.subarray(end + 4).toString().trim() || head.toString().trim();
          return reject(status === "403" ? new PermissionDenied(`${target}: ${why}`) : new LunaError(`${target}: ${why}`));
        }
        resolve(s); // a proxy sends nothing after 200 until the client speaks, so no bytes are lost
      };
      s.on("data", onData);
    });
    return new Promise<{ status: number; headers: Record<string, string>; body: Uint8Array }>((resolve, reject) => {
      const agent = new https.Agent({ keepAlive: false });
      agent.createConnection = () => tls.connect({ socket: raw, servername: u.hostname }); // the tunnel, never a DNS lookup
      const req = https.request(url, { method, headers, timeout: timeoutMs, agent }, (res) => {
        const chunks: Buffer[] = [];
        res.on("data", (c) => chunks.push(c));
        res.on("end", () => {
          const h: Record<string, string> = {};
          for (const [k, v] of Object.entries(res.headers)) h[k] = Array.isArray(v) ? v.join(", ") : String(v ?? "");
          resolve({ status: res.statusCode ?? 0, headers: h, body: Buffer.concat(chunks) });
        });
      });
      req.on("timeout", () => req.destroy(new Timeout(`${url}: no answer in ${timeoutMs} ms`)));
      req.on("error", reject);
      req.end(body);
    });
  }

  // A link is one connection, one call at a time, through the runtime's broker.
  linkCall(use: string, args: Json): Promise<Json> {
    const prev = this.linkQueue.get(use) ?? Promise.resolve();
    const run = prev.catch(() => {}).then(() => this.linkOnce(use, args));
    this.linkQueue.set(use, run);
    return run;
  }

  private async linkOnce(use: string, args: Json): Promise<Json> {
    let conn = this.links.get(use);
    if (!conn) {
      conn = Frames.connect(path.join(process.env.CAPSULE_LINKS ?? "", use));
      this.links.set(use, conn);
    }
    let f: any;
    try {
      const c = await conn;
      const id = ++this.seq;
      c.send("call", id, { method: use.slice(use.lastIndexOf(":") + 1), body: args });
      f = await Promise.race([c.recv(), new Promise((r) => setTimeout(() => r(null), 10_000))]);
    } catch {
      f = null;
    }
    if (!f) {
      this.links.delete(use);
      throw new LinkUnavailable(`${use}: not approved, not in [links].uses, or the link dropped`);
    }
    const r = f.b ?? {};
    if (!r.ok) {
      const err = r.error ?? "failed";
      if (err === "link_unavailable" || err === "timeout") throw new LinkUnavailable(`${use}: ${err}`);
      throw new LunaError(`${use}: ${err}`);
    }
    return r.body ?? null;
  }
}

export async function runLive(bot: Bot): Promise<void> {
  const ctl = await Frames.connect(process.env.CAPSULE_CONTROL!);
  ctl.send("hello", 0, { spec: "0.1", pid: process.pid });
  if ((await ctl.recv())?.t !== "welcome") throw new Error("the runtime didn't welcome this capsule");

  const t = new Live(process.env.CAPSULE_HOST!);
  bot.transport = t;
  let hello: Json;
  try {
    hello = await t.call("Hello", "HelloRequest", "HelloReply", { api: API, sdk: SDK });
  } catch (e) {
    console.error(`Luna refused the plugin API: ${(e as Error).message}`);
    process.exit(1);
  }
  bot.id = hello.id;
  bot.dev = hello.dev;
  bot.loadConfig(parse(hello.config_json) ?? {});

  let stopping = false;
  const fatal = (what: string) => (err: GrpcError | null) => {
    if (!stopping) {
      console.error(`${what} ended: ${err?.message ?? "closed"}`);
      process.exit(1); // the supervisor restarts the capsule
    }
  };
  const types = bot.describe().events as string[];
  if (types.length) {
    t.c.serverStream("Subscribe", "SubscribeRequest", "Event", { types },
      (e) => { bot.dispatch(makeEvent(e.id, e.type, e.source, e.ts_ms, e.simulated, parse(e.data_json) ?? {})); },
      fatal("event stream"));
  }
  if (bot.cliCmds.size + bot.chatCmds.size) {
    const send = t.c.bidi("Commands", "CommandResult", "Command", async (c) => {
      const r: Json = { id: c.id };
      try {
        const ev = c.event ? makeEvent(c.event.id, c.event.type, c.event.source, c.event.ts_ms, c.event.simulated,
          parse(c.event.data_json) ?? {}) : undefined;
        const out = await bot.command(c.kind, c.name, parse(c.args_json) ?? {}, ev);
        r.text = out.text;
        if (out.data !== undefined) r.data_json = json(out.data);
      } catch (e) {
        r.error = (e as Error)?.message || String(e);
      }
      send(r);
    }, fatal("command stream"));
  }

  let timer: NodeJS.Timeout | undefined;
  const pump = async () => {
    clearTimeout(timer);
    const wait = await bot._runDue();
    if (wait >= 0) timer = setTimeout(pump, Math.min(wait, 2 ** 31 - 1));
  };
  bot.wake = () => { setImmediate(pump); };
  await bot.start();
  pump();
  ctl.send("ready");

  for (;;) {
    const f = await ctl.recv();
    if (!f || f.t === "shutdown") break;
    if (f.t === "ping") ctl.send("pong", f.id);
    else if (f.t === "call") {
      const b = f.b ?? {};
      const fn = bot.provided.get(b.link);
      (async () => {
        if (!fn) return ctl.send("result", f.id, { ok: false, error: "no such link" });
        try {
          const out = await fn(b.body ?? {}, b.caller ?? "");
          ctl.send("result", f.id, { ok: true, body: out ?? null });
        } catch (e) {
          ctl.send("result", f.id, { ok: false, error: (e as Error)?.message || String(e) });
        }
      })();
    }
  }
  stopping = true;
  clearTimeout(timer);
  for (const fn of bot.stops) await bot.call(fn);
  t.c.close();
  process.exit(0);
}
