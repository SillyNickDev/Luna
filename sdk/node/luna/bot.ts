// The bot object every plugin uses: handlers, commands, config and the API groups. It
// talks to Luna through a Transport: the real one in live.ts, a fake in testing.ts.
import { AsyncLocalStorage } from "node:async_hooks";

export class LunaError extends Error { name = "LunaError"; }
export class PermissionDenied extends LunaError { name = "PermissionDenied"; }
export class RateLimited extends LunaError { name = "RateLimited"; }
export class NotConnected extends LunaError { name = "NotConnected"; }
export class Timeout extends LunaError { name = "Timeout"; }
export class LinkUnavailable extends LunaError { name = "LinkUnavailable"; }
export class ConfigError extends LunaError { name = "ConfigError"; }

export type Json = any;

export interface User {
  id: string;
  login: string;
  name: string;
  roles: string[];
  rank?: string; // chat messages: viewer, moderator, luna-mod, head-mod, broadcaster
}

const RANKS = ["viewer", "moderator", "luna-mod", "head-mod", "broadcaster"];

/** Whether a user's rank is at least rank ("moderator", "luna-mod", ...). */
export function rankAtLeast(u: User | undefined, rank: string): boolean {
  return !!u && RANKS.indexOf(u.rank ?? "viewer") >= RANKS.indexOf(rank);
}

/** An event: id, type, source, ts, simulated, plus its payload's fields (ev.user, ev.bpm, ...). */
export type Event = {
  id: string;
  type: string;
  source: string;
  ts: Date;
  simulated: boolean;
  data: Json;
  user?: User;
  [field: string]: any;
};

export function makeEvent(id: string, type: string, source: string, tsMs: number, simulated: boolean, data: Json): Event {
  return { ...data, id, type, source, ts: new Date(tsMs), simulated, data };
}

/** The transport: one method per plugin API call, JSON already decoded. */
export interface Transport {
  chatSend(text: string, simulated: boolean, replyTo: string, mode: string, to: string): Promise<void>;
  mod(req: { action: string; user_id?: string; seconds?: number; reason?: string; message_id?: string; on?: boolean; simulated?: boolean }): Promise<void>;
  twitch(call: string, user: string, args: Json, simulated: boolean): Promise<Json>;
  viewer(user: string): Promise<Json>;
  storeGet(key: string): Promise<Json | undefined>;
  storeSet(key: string, value: Json, ttlMs: number): Promise<void>;
  storeDelete(key: string): Promise<void>;
  storeIncr(key: string, by: number): Promise<number>;
  storeKeys(prefix: string): Promise<string[]>;
  storeBatch(sets: [string, Json, number][], deletes: string[]): Promise<void>;
  log(level: string, msg: string): Promise<void>;
  emit(name: string, data: Json): Promise<void>;
  linkCall(use: string, args: Json): Promise<Json>;
  osc(req: { action: string; address?: string; value?: Json; type?: string; text?: string; name?: string; simulated?: boolean }): Promise<Json>;
  hr(minutes: number): Promise<{ bpm: number; atMs: number; history: { atMs: number; bpm: number }[] }>;
}

const current = new AsyncLocalStorage<Event | undefined>();
const simulated = () => !!current.getStore()?.simulated;

/** "90s", "5m", "1h30m", "2d" or milliseconds, as milliseconds. */
export function ms(d: string | number): number {
  if (typeof d === "number") return d;
  const re = /(\d+(?:\.\d+)?)(ms|s|m|h|d)/gy;
  let total = 0, m: RegExpExecArray | null, end = 0;
  while ((m = re.exec(d))) {
    total += Number(m[1]) * { ms: 1, s: 1e3, m: 60e3, h: 3600e3, d: 86400e3 }[m[2] as "s"];
    end = re.lastIndex;
  }
  if (!d || end !== d.length) throw new LunaError(`can't read the duration ${JSON.stringify(d)}; try "30s", "5m" or "1h"`);
  return total;
}

export class Ctx {
  event?: Event;
  user?: User;
  private bot: Bot;
  constructor(bot: Bot, event?: Event) {
    this.bot = bot;
    this.event = event;
    this.user = event?.user;
  }
  /** Answer in chat, threaded to the message that ran the command. */
  reply(text: string): Promise<void> {
    return this.bot._t().chatSend(String(text), !!this.event?.simulated, this.event?.message_id ?? "", "", "");
  }
}

type Handler = { pattern: string; fn: (ev: Event) => unknown; when?: (ev: Event) => boolean; once: boolean; done: boolean;
  dropIfBusy: boolean; busy: number; chain: Promise<void> };
export type ArgSpec = { name: string; type: "str" | "int" | "float" | "bool"; optional?: boolean };
export type CliSpec = { name: string; summary?: string; args?: ArgSpec[]; flags?: ArgSpec[]; mutates?: boolean };
export type ChatSpec = { name: string; aliases?: string[]; capability?: string; cooldown?: string; userCooldown?: string };
type Timer = { at: number; every: number; fn: () => unknown; ev?: Event; seq: number; cancelled: boolean };

function match(pattern: string, type: string): boolean {
  return pattern === type || (pattern.endsWith(".*") && type.startsWith(pattern.slice(0, -1)));
}

function configType(v: unknown): string {
  if (typeof v === "boolean") return "bool";
  if (typeof v === "string") return "str";
  if (typeof v === "number") return Number.isInteger(v) ? "int" : "float";
  if (Array.isArray(v)) return "list";
  throw new ConfigError(`config defaults must be numbers, strings, booleans or lists of tables, not ${typeof v}`);
}

export class Bot<C extends Record<string, any> = Record<string, any>> {
  id = "";
  dev = false;
  config: C = {} as C;
  clock: () => number = Date.now;
  transport?: Transport;

  readonly handlers: Handler[] = [];
  readonly cliCmds = new Map<string, CliSpec & { fn: (args: any) => unknown }>();
  readonly chatCmds = new Map<string, ChatSpec & { fn: (ctx: Ctx, args: string[]) => unknown }>();
  private defaults?: Record<string, any>;
  private starts: (() => unknown)[] = [];
  readonly stops: (() => unknown)[] = [];
  private queue: Timer[] = [];
  private seq = 0;
  wake: () => void = () => {};
  readonly provided = new Map<string, (args: Json, caller: string) => unknown>();
  readonly uses = new Set<string>();

  _t(): Transport {
    if (!this.transport) throw new NotConnected("not connected to Luna yet; call bot.run()");
    return this.transport;
  }

  // ---------- declarations ----------

  /** Handle events matching pattern ("twitch.channel.follow" or "twitch.*"), in order. */
  on(pattern: string, fn: (ev: Event) => unknown, opts: { when?: (ev: Event) => boolean; dropIfBusy?: boolean } = {}): void {
    this.handlers.push({ pattern, fn, when: opts.when, once: false, done: false, dropIfBusy: !!opts.dropIfBusy, busy: 0, chain: Promise.resolve() });
  }

  /** on, for the first matching event only. */
  once(pattern: string, fn: (ev: Event) => unknown, opts: { when?: (ev: Event) => boolean } = {}): void {
    this.on(pattern, fn, opts);
    this.handlers[this.handlers.length - 1].once = true;
  }

  /** `luna <plugin> <name>`: fn gets the parsed args and flags; return a string, or data for a table. */
  cli(spec: CliSpec, fn: (args: Record<string, any>) => unknown): void {
    this.cliCmds.set(spec.name, { args: [], flags: [], mutates: false, summary: "", ...spec, fn });
  }

  /** !name in chat: fn gets the words after it. Luna checks capability and cooldowns first. */
  chatCommand(spec: ChatSpec | string, fn: (ctx: Ctx, args: string[]) => unknown): void {
    const s = typeof spec === "string" ? { name: spec } : spec;
    this.chatCmds.set(s.name, { aliases: [], ...s, fn });
  }

  /** Settings with their defaults: each becomes a [config] entry (int, float, str, bool or list of tables). */
  useConfig<T extends Record<string, any>>(defaults: T): Bot<T> {
    for (const v of Object.values(defaults)) configType(v);
    this.defaults = defaults;
    this.config = { ...defaults } as any;
    return this as any;
  }

  onStart(fn: () => unknown): void { this.starts.push(fn); }
  onStop(fn: () => unknown): void { this.stops.push(fn); }

  // ---------- the API groups ----------

  readonly chat = {
    send: (text: string) => this._t().chatSend(String(text), simulated(), "", "", ""),
    announce: (text: string) => this._t().chatSend(String(text), simulated(), "", "announce", ""),
    whisper: (user: User | string, text: string) =>
      this._t().chatSend(String(text), simulated(), "", "whisper", typeof user === "string" ? user : user.id),
  };

  readonly mod = {
    timeout: (user: User | string, seconds = 600, reason = "") => this.modDo({ action: "timeout", user_id: uid(user), seconds, reason }),
    ban: (user: User | string, reason = "") => this.modDo({ action: "ban", user_id: uid(user), reason }),
    unban: (user: User | string) => this.modDo({ action: "unban", user_id: uid(user) }),
    delete: (messageId: string) => this.modDo({ action: "delete", message_id: messageId }),
    warn: (user: User | string, reason: string) => this.modDo({ action: "warn", user_id: uid(user), reason }),
    shield: (on: boolean) => this.modDo({ action: "shield", on }),
    shoutout: (channel: User | string) => this.modDo({ action: "shoutout", user_id: uid(channel) }),
  };

  private modDo(req: Parameters<Transport["mod"]>[0]): Promise<void> {
    return this._t().mod({ ...req, simulated: simulated() });
  }

  readonly store = {
    get: async <T = any>(key: string, fallback?: T): Promise<T> => {
      const v = await this._t().storeGet(key);
      return v === undefined ? (fallback as T) : v;
    },
    /** ttl: "10m", ms, or none to keep it. */
    set: (key: string, value: Json, opts: { ttl?: string | number } = {}) =>
      this._t().storeSet(key, value, opts.ttl === undefined ? 0 : ms(opts.ttl)),
    delete: (key: string) => this._t().storeDelete(key),
    incr: (key: string, by = 1) => this._t().storeIncr(key, by),
    keys: (prefix = "") => this._t().storeKeys(prefix),
    /** Sets and deletes, applied atomically. */
    batch: (ops: { set?: [string, Json, (string | number)?][]; delete?: string[] }) =>
      this._t().storeBatch((ops.set ?? []).map(([k, v, t]) => [k, v, t === undefined ? 0 : ms(t)]), ops.delete ?? []),
  };

  private tw(call: string, user = "", args: Json = undefined): Promise<Json> {
    return this._t().twitch(call, user, args, simulated());
  }

  readonly twitch = {
    user: (loginOrId: string) => this.tw("user", String(loginOrId).replace(/^@/, "")),
    channel: (id = "") => this.tw("channel", id),
    stream: (id = "") => this.tw("stream", id),
    followedAt: async (user: User | string): Promise<Date | null> => {
      const f = await this.tw("follow", uid(user));
      return f ? new Date(f.followed_at) : null;
    },
    categories: async (query: string) => (await this.tw("categories", "", { query })) ?? [],
    schedule: async () => (await this.tw("schedule")) ?? [],
    streams: async (logins: string[]) => (await this.tw("streams", "", { logins })) ?? [],
    setChannel: (o: { title?: string; gameId?: string }) => this.tw("set_channel", "", { title: o.title ?? "", game_id: o.gameId ?? "" }),
    clip: () => this.tw("clip"),
    marker: (description = "") => this.tw("marker", "", { description }),
    ads: () => this.tw("ads"),
    snoozeAd: () => this.tw("snooze"),
    raid: (toId: string) => this.tw("raid", "", { to_id: toId }),
    unraid: () => this.tw("unraid"),
  };

  readonly viewers = {
    get: (user: User | string) => this._t().viewer(typeof user === "string" ? user : user.id),
  };

  readonly osc = {
    /** Set an address to a boolean, number or string. Integers go as Int unless the avatar says Float. */
    send: (address: string, value: boolean | number | string) =>
      this._t().osc({ action: "send", address, value, type: typeof value === "boolean" ? "bool" : typeof value === "string" ? "string" : "",
        simulated: simulated() }),
    setParam: (name: string, value: boolean | number | string) => this.osc.send("/avatar/parameters/" + name, value),
    chatbox: (text: string) => this._t().osc({ action: "chatbox", text: String(text), simulated: simulated() }),
    param: (name: string) => this._t().osc({ action: "param", name }),
    avatar: () => this._t().osc({ action: "avatar" }),
  };

  readonly hr = {
    bpm: async () => (await this._t().hr(0)).bpm,
    history: async (minutes: number) => (await this._t().hr(minutes)).history.map((s) => ({ at: new Date(s.atMs), bpm: s.bpm })),
  };

  readonly links = {
    /** Answer calls on link name (also in [links].provides); caller is the calling plugin's ID. */
    provide: (name: string, fn: (args: Json, caller: string) => unknown) => { this.provided.set(name, fn); },
    /** Declare links this plugin calls, and get a caller: const overlay = bot.links.use("dev.sable.overlay", "show"). */
    use: (provider: string, ...names: string[]): Record<string, (args?: Json) => Promise<Json>> => {
      const out: Record<string, (args?: Json) => Promise<Json>> = {};
      for (const n of names) {
        this.uses.add(`${provider}:${n}`);
        out[n] = (args = {}) => this._t().linkCall(`${provider}:${n}`, args);
      }
      return out;
    },
  };

  readonly timers = {
    /** Run fn once after d; returns a cancel function. */
    after: (d: string | number, fn: () => unknown) => this.schedule(ms(d), 0, fn, current.getStore()),
    /** Run fn every d, first after d; returns a cancel function. */
    every: (d: string | number, fn: () => unknown) => this.schedule(ms(d), ms(d), fn),
  };

  readonly log = {
    debug: (msg: string) => this._t().log("debug", msg),
    info: (msg: string) => this._t().log("info", msg),
    warn: (msg: string) => this._t().log("warn", msg),
    error: (msg: string) => this._t().log("error", msg),
  };

  /** Publish plugin.<id>.<name> with data. */
  emit(name: string, data: Json = {}): Promise<void> { return this._t().emit(name, data); }

  // ---------- running ----------

  private schedule(delay: number, every: number, fn: () => unknown, ev?: Event): () => void {
    if (every && every < 1000) throw new LunaError("timers repeat at most once a second");
    const t: Timer = { at: this.clock() + delay, every, fn, ev, seq: this.seq++, cancelled: false };
    if (!this.started) {
      this.declared.push(t); // declared at load: counted from when the bot starts
    } else {
      this.queue.push(t);
      this.wake();
    }
    return () => { t.cancelled = true; };
  }

  private declared: Timer[] = [];
  private started = false;

  /** Runs due timers; returns ms until the next one, or -1 if none. */
  async _runDue(): Promise<number> {
    for (;;) {
      this.queue = this.queue.filter((t) => !t.cancelled);
      this.queue.sort((a, b) => a.at - b.at || a.seq - b.seq);
      const t = this.queue[0];
      if (!t) return -1;
      const now = this.clock();
      if (t.at > now) return t.at - now;
      if (t.every) t.at += t.every;
      else t.cancelled = true;
      await this.call(t.fn, t.ev);
    }
  }

  /** Run plugin code with ev as the current event; errors are logged, not thrown. */
  async call(fn: () => unknown, ev?: Event): Promise<unknown> {
    try {
      return await current.run(ev, fn);
    } catch (e) {
      if (this.crashOnError) throw e;
      const where = ev ? ` handling ${ev.type} ${ev.id}` : "";
      this.transport?.log("error", `error${where}: ${(e as Error)?.stack ?? e}`).catch(() => {});
      return e;
    }
  }

  crashOnError = false;

  /** Drop every timer but the declared ones: the test harness starts each test clean. */
  resetTimers(): void {
    this.queue = [];
    this.started = false;
  }

  /** Hand an event to every matching handler; each handler sees its events in order. */
  dispatch(ev: Event): Promise<void> {
    const runs: Promise<void>[] = [];
    for (const h of this.handlers) {
      if (h.done || !match(h.pattern, ev.type)) continue;
      if (h.when) {
        try {
          if (!h.when(ev)) continue;
        } catch {
          continue;
        }
      }
      if (h.dropIfBusy && h.busy) continue;
      if (h.once) h.done = true;
      h.busy++;
      h.chain = h.chain.then(async () => {
        try {
          await this.call(() => h.fn(ev), ev);
        } finally {
          h.busy--;
        }
      });
      runs.push(h.chain);
    }
    return Promise.all(runs).then(() => {});
  }

  /** Run a CLI or chat command; returns data or text. */
  async command(kind: string, name: string, args: Json, ev?: Event): Promise<{ data?: Json; text: string }> {
    let res: unknown;
    if (kind === "cli") {
      const c = this.cliCmds.get(name);
      if (!c) throw new LunaError(`no CLI command ${JSON.stringify(name)}`);
      res = await current.run(undefined, () => c.fn(args ?? {}));
    } else {
      const c = this.chatCmds.get(name);
      if (!c) throw new LunaError(`no chat command ${JSON.stringify(name)}`);
      res = await current.run(ev, () => c.fn(new Ctx(this, ev), args?.args ?? []));
    }
    if (res === undefined || res === null || typeof res === "string") return { text: (res as string) ?? "" };
    return { data: res, text: "" };
  }

  /** Apply Luna's effective config over the defaults, checking each type. */
  loadConfig(values: Record<string, Json>): void {
    if (!this.defaults) {
      this.config = values as any;
      return;
    }
    const out: Record<string, any> = {};
    for (const [k, def] of Object.entries(this.defaults)) {
      const v = k in values ? values[k] : def;
      const t = configType(def);
      const ok = t === "list" ? Array.isArray(v) && v.every((x) => x && typeof x === "object" && !Array.isArray(x))
        : t === "int" ? Number.isInteger(v) : t === "float" ? typeof v === "number" : typeof v === (t === "str" ? "string" : "boolean");
      if (!ok) throw new ConfigError(`config ${k}: expected ${t}, got ${JSON.stringify(v)}`);
      out[k] = v;
    }
    this.config = out as any;
  }

  async start(): Promise<void> {
    this.started = true;
    const now = this.clock();
    for (const d of this.declared) {
      if (!d.cancelled) this.queue.push({ ...d, at: now + (d.every || 0), seq: this.seq++ });
    }
    this.wake();
    for (const fn of this.starts) await this.call(fn);
  }

  /** What luna capsule dev writes into plugin.toml. */
  describe(): Json {
    const events = [...new Set(this.handlers.map((h) => h.pattern))].sort();
    const cli = [...this.cliCmds.values()].map(({ fn, ...c }) => c).sort((a, b) => a.name.localeCompare(b.name));
    const chat = [...this.chatCmds.values()].map((c) => ({ name: c.name, aliases: c.aliases ?? [], capability: c.capability ?? null,
      cooldown: c.cooldown ?? null, user_cooldown: c.userCooldown ?? null })).sort((a, b) => a.name.localeCompare(b.name));
    const config: Record<string, any> = {};
    for (const [k, v] of Object.entries(this.defaults ?? {})) config[k] = { type: configType(v), default: v };
    return { events, cli, chat, config, links: { provides: [...this.provided.keys()].sort(), uses: [...this.uses].sort() } };
  }

  /** Connect to Luna and run until it says stop. */
  async run(): Promise<void> {
    if (process.env.LUNA_TESTING) return;
    if (process.env.LUNA_DESCRIBE) {
      process.stdout.write(JSON.stringify(this.describe()) + "\n");
      return;
    }
    if (!process.env.CAPSULE_CONTROL) {
      console.error("this is a Luna plugin; run it with: luna capsule dev");
      process.exit(1);
    }
    const { runLive } = await import("./live.ts");
    await runLive(this);
  }
}

function uid(u: User | string): string {
  return typeof u === "string" ? u : u.id;
}

export const bot = new Bot();
