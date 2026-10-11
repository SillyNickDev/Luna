// Run a plugin in-process against a fake Luna: no daemon, capsule or Twitch account.
//
//   import { harness } from "@luna/sdk/testing";
//   import "./main.ts";
//   test("welcome", async () => {
//     const t = await harness();
//     await t.emit("twitch.channel.follow", { user: "fox" });
//     assert.deepEqual(t.chat.sent, ["Welcome to the pack, fox!"]);
//   });
//
// Permissions from ./plugin.toml are enforced, and chat commands only reach users whose
// rank has the command's capability. Time is fake: await t.clock.advance("2m").
import fs from "node:fs";
import { bot as defaultBot, type Bot, type Event, type Json, type Transport, LinkUnavailable, LunaError, makeEvent, ms, PermissionDenied } from "./bot.ts";
import { parseToml } from "./toml.ts";

process.env.LUNA_TESTING = "1";

// Luna's default capability table: the lowest rank holding each capability.
const MIN_RANK: Record<string, string> = {
  "chat.commands.use": "viewer", "mod.delete": "moderator", "mod.timeout": "moderator", "mod.warn": "moderator",
  "mod.ban": "moderator", "mod.undo.own": "moderator", "mod.notes": "moderator", "mod.shield": "moderator",
  "stream.shoutout": "moderator", "commands.edit": "moderator", "mod.undo.others": "luna-mod",
  "filters.edit": "luna-mod", "giveaway.run": "luna-mod", "points.adjust": "luna-mod", "stream.edit": "luna-mod",
  "luna.chat_cli": "luna-mod", "mod.lock": "head-mod", "perms.approve_luna_mods": "head-mod",
};
const RANKS = ["viewer", "moderator", "luna-mod", "head-mod", "broadcaster"];
const TWITCH_PERM: Record<string, string> = {
  user: "read", follow: "read", channel: "read", stream: "read", categories: "read", schedule: "read", streams: "read",
  set_channel: "edit", clip: "clips", marker: "markers", ads: "ads", snooze: "ads", raid: "raid", unraid: "raid",
};

/** Whether a rank holds a capability under Luna's defaults. */
export function can(rank: string, capability: string): boolean {
  if (rank === "broadcaster") return true;
  const want = MIN_RANK[capability];
  return want !== undefined && RANKS.indexOf(rank) >= RANKS.indexOf(want);
}

export function user(login: string, roles: string[] = []): Json {
  const rank = ["broadcaster", "head-mod", "luna-mod", "moderator"].find((r) => roles.includes(r)) ?? "viewer";
  if (rank === "head-mod" || rank === "luna-mod") roles = [...roles, "moderator"];
  return { id: "test-" + login, login, name: login, roles, rank };
}

const allowed = (perms: string[], type: string) => perms.some((p) => p === type || (p.endsWith(".*") && type.startsWith(p.slice(0, -1))));

export class Harness {
  bot: Bot;
  perms: Json | null;
  now = Date.UTC(2026, 0, 1, 12);
  chat = { sent: [] as string[], messages: [] as { text: string; mode: string; to: string; replyTo: string }[] };
  mod = { actions: [] as Json[] };
  twitch = { users: {} as Record<string, Json>, channels: {} as Record<string, Json>, stream: { live: false } as Json,
    follows: {} as Record<string, string>, categories: [] as Json[], schedule: [] as Json[], live: {} as Record<string, Json>,
    ads: { next_ad_at: null, duration_s: 0, snooze_count: 0 } as Json, writes: [] as [string, Json][] };
  viewers: Record<string, Json> = {};
  store = new Map<string, { v: Json; exp: number }>();
  osc = { sent: [] as [string, Json][], chatbox: [] as string[], params: {} as Record<string, Json>,
    avatar: { id: "", name: "", params: [] } as Json };
  logs: [string, string][] = [];
  emitted: [string, Json][] = [];
  private hrs: { atMs: number; bpm: number }[] = [];
  private links = new Map<string, Record<string, (args: Json) => unknown>>();
  private down = new Set<string>();
  private cooldowns = new Map<string, number>();
  private n = 0;

  constructor(b: Bot, perms: Json | null) {
    this.bot = b;
    this.perms = perms;
  }

  clock = {
    now: () => this.now,
    /** Move time forward, running every timer that comes due on the way. */
    advance: async (d: string | number) => {
      const end = this.now + ms(d);
      for (;;) {
        const wait = await this.bot._runDue();
        if (wait < 0 || this.now + wait > end) break;
        this.now += wait;
      }
      this.now = end;
      await this.bot._runDue();
    },
  };

  private id(prefix: string) { return `${prefix}-${++this.n}`; }

  /** Fire an event at the plugin; { user: "fox" } is shorthand for a viewer named fox. */
  async emit(type: string, data: Json = {}, simulated = false): Promise<Event> {
    if (typeof data.user === "string") data = { ...data, user: user(data.user) };
    if (type === "hr.update" && Number.isInteger(data.bpm)) {
      this.hrs = [...this.hrs.filter((s) => s.atMs >= this.now - 3600e3), { atMs: this.now, bpm: data.bpm }];
    }
    const ev = makeEvent(this.id("test"), type, "test", this.now, simulated, data);
    await this.bot.dispatch(ev);
    return ev;
  }

  /** Type a chat line as a user; returns what the plugin said in response. Roles are Twitch badges
   * ("sub", "vip", "moderator", "broadcaster") or Luna ranks ("luna-mod", "head-mod"). */
  async chatCommand(line: string, opts: { user?: string; roles?: string[] } = {}): Promise<string[]> {
    const before = this.chat.sent.length;
    const u = user(opts.user ?? "testviewer", opts.roles ?? []);
    const ev = makeEvent(this.id("test"), "twitch.chat.message", "test", this.now, false, { user: u, text: line, message_id: this.id("msg") });
    await this.bot.dispatch(ev);
    const words = line.startsWith("!") ? line.trim().split(/\s+/) : [];
    const name = words[0]?.slice(1).toLowerCase() ?? "";
    const cmd = this.bot.chatCmds.get(name) ?? [...this.bot.chatCmds.values()].find((c) => c.aliases?.includes(name));
    if (!cmd || !can(u.rank, cmd.capability ?? "chat.commands.use")) return this.chat.sent.slice(before);
    const keys: [string, string | undefined][] = [[cmd.name, cmd.cooldown], [`${cmd.name}\0${u.id}`, cmd.userCooldown]];
    if (keys.some(([k, cd]) => cd && (this.cooldowns.get(k) ?? -Infinity) + ms(cd) > this.now)) return this.chat.sent.slice(before);
    for (const [k, cd] of keys) if (cd) this.cooldowns.set(k, this.now);
    await this.bot.command("chat", cmd.name, { args: words.slice(1) }, ev);
    return this.chat.sent.slice(before);
  }

  /** Run `luna <plugin> name` with parsed args; returns its data or text. */
  async cli(name: string, args: Json = {}): Promise<Json> {
    const r = await this.bot.command("cli", name, args);
    return r.data !== undefined ? r.data : r.text;
  }

  /** Fake a link provider: t.link("luna.points", { spend: (args) => ({ balance: 5 }) }). */
  link(provider: string, fns: Record<string, (args: Json) => unknown>): void {
    this.links.set(provider, fns);
    this.down.delete(provider);
  }

  linkDown(provider: string): void { this.down.add(provider); }

  need(group: string, item: string, line = `"${item}" to [permissions].${group}`): void {
    if (this.perms && !(this.perms[group] ?? []).includes(item)) throw new PermissionDenied(`add ${line} in plugin.toml`);
  }

  transport(): Transport {
    const h = this;
    const store = () => {
      if (h.perms && !h.perms.store) throw new PermissionDenied("add store = true to [permissions] in plugin.toml");
      for (const [k, { exp }] of h.store) if (exp && exp <= h.now) h.store.delete(k);
      return h.store;
    };
    const copy = (v: Json) => (v === undefined ? undefined : JSON.parse(JSON.stringify(v)));
    return {
      async chatSend(text, _sim, replyTo, mode, to) {
        h.need("chat", mode || "send");
        h.chat.sent.push(text);
        h.chat.messages.push({ text, mode, to, replyTo });
      },
      async mod(req) {
        const need = req.action === "unban" ? ((h.perms?.mod ?? []).includes("timeout") || !h.perms ? "timeout" : "ban") : req.action;
        h.need("mod", need);
        if (req.action === "timeout" && !((req.seconds ?? 0) >= 1 && (req.seconds ?? 0) <= 1209600)) throw new LunaError("a timeout is 1 s to 14 days");
        h.mod.actions.push(req);
      },
      async twitch(call, u, args) {
        const need = TWITCH_PERM[call];
        if (!need) throw new LunaError(`unknown twitch call ${JSON.stringify(call)}`);
        h.need("twitch", need);
        const tw = h.twitch;
        if (need !== "read" && call !== "ads") {
          tw.writes.push([call, args ?? {}]);
          return ({ clip: { id: "clip1", url: "https://clips.twitch.tv/clip1", edit_url: "" }, marker: { id: "m1", position_s: 0 }, snooze: tw.ads } as Json)[call] ?? null;
        }
        switch (call) {
          case "user": return copy(Object.values(tw.users).find((x) => [String(x.login).toLowerCase(), String(x.id)].includes(u.toLowerCase())) ?? null);
          case "channel": return copy(tw.channels[u] ?? null);
          case "stream": return copy(tw.stream);
          case "follow": return u in tw.follows ? { followed_at: tw.follows[u] } : null;
          case "categories": return tw.categories.filter((c) => c.name.toLowerCase().includes(String(args?.query ?? "").toLowerCase()));
          case "schedule": return copy(tw.schedule);
          case "streams": return (args?.logins ?? []).filter((l: string) => l in tw.live).map((l: string) => tw.live[l]);
          case "ads": return copy(tw.ads);
        }
        return null;
      },
      async viewer(u) {
        if (h.perms && !h.perms.viewers) throw new PermissionDenied("add viewers = true to [permissions] in plugin.toml");
        return copy(h.viewers[u] ?? Object.values(h.viewers).find((v) => v.login === u) ?? null);
      },
      async storeGet(k) { return copy(store().get(k)?.v); },
      async storeSet(k, v, ttl) { store().set(k, { v: copy(v), exp: ttl ? h.now + ttl : 0 }); },
      async storeDelete(k) { store().delete(k); },
      async storeIncr(k, by) {
        const s = store();
        const cur = s.get(k);
        const n = cur ? cur.v : 0;
        if (!Number.isInteger(n)) throw new LunaError(`store: ${JSON.stringify(k)} doesn't hold an integer`);
        s.set(k, { v: n + by, exp: cur?.exp ?? 0 });
        return n + by;
      },
      async storeKeys(prefix) { return [...store().keys()].filter((k) => k.startsWith(prefix)).sort(); },
      async storeBatch(sets, deletes) {
        const s = store();
        for (const [k, v, ttl] of sets) s.set(k, { v: copy(v), exp: ttl ? h.now + ttl : 0 });
        for (const k of deletes) s.delete(k);
      },
      async log(level, msg) { h.logs.push([level, msg]); },
      async emit(name, data) { h.emitted.push([name, copy(data)]); },
      async linkCall(use, args) {
        const provider = use.slice(0, use.lastIndexOf(":")), method = use.slice(use.lastIndexOf(":") + 1);
        if (h.down.has(provider) || !h.links.has(provider)) throw new LinkUnavailable(`${use}: link_unavailable`);
        const fn = h.links.get(provider)![method];
        if (!fn) throw new LunaError(`${use}: the fake provider has no ${method}`);
        return copy(await fn(copy(args)));
      },
      async osc(req) {
        const have: string[] | undefined = h.perms ? (h.perms.osc ?? []) : undefined;
        if (req.action === "send") {
          const ok = !have || have.some((p) => p === "send:" + req.address || (p.endsWith("*") && ("send:" + req.address).startsWith(p.slice(0, -1))));
          if (!ok) throw new PermissionDenied(`add "send:${req.address}" to [permissions].osc in plugin.toml`);
          h.osc.sent.push([req.address!, req.value]);
          return null;
        }
        h.need("osc", req.action === "chatbox" ? "chatbox" : "read");
        if (req.action === "chatbox") {
          h.osc.chatbox.push(String(req.text).slice(0, 144));
          return null;
        }
        return copy(req.action === "param" ? (h.osc.params[req.name!] ?? null) : h.osc.avatar);
      },
      async hr(minutes) {
        if (h.perms && !allowed(h.perms.events ?? [], "hr.update")) throw new PermissionDenied('add "hr.update" to [permissions].events in plugin.toml');
        const last = h.hrs[h.hrs.length - 1];
        if (!last) return { bpm: 0, atMs: 0, history: [] };
        return { bpm: last.bpm, atMs: last.atMs, history: minutes > 0 ? h.hrs.filter((s) => s.atMs >= last.atMs - minutes * 60e3) : [] };
      },
    };
  }
}

/** A fresh fake Luna for the plugin's bot, with config overrides like luna.toml's [plugins.<name>]. */
export async function harness(opts: { config?: Json; manifest?: string; bot?: Bot } = {}): Promise<Harness> {
  const b = opts.bot ?? defaultBot;
  const path = opts.manifest ?? "plugin.toml";
  const perms = fs.existsSync(path) ? (parseToml(fs.readFileSync(path, "utf8")).permissions ?? {}) : null;
  const h = new Harness(b, perms);
  b.transport = h.transport();
  b.clock = () => h.now;
  b.id = "dev.test.plugin";
  b.crashOnError = true;
  b.wake = () => {};
  for (const hd of b.handlers) hd.done = false;
  for (const t of b.describe().events as string[]) {
    if (perms && !allowed(perms.events ?? [], t)) throw new PermissionDenied(`add "${t}" to [permissions].events in plugin.toml`);
  }
  b.resetTimers();
  b.loadConfig(opts.config ?? {});
  await b.start();
  return h;
}
