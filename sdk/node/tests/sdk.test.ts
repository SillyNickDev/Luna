import assert from "node:assert/strict";
import { test } from "node:test";
import { harness } from "@luna/sdk/testing";
import { bot, PermissionDenied } from "@luna/sdk";
import { decode, encode } from "../luna/cbor.ts";
import * as proto from "../luna/proto.ts";
import { parseToml } from "../luna/toml.ts";

process.chdir(new URL("./example/", import.meta.url).pathname);
await import("./example/main.ts");

test("alerts with cooldown, and the link going down", async () => {
  const t = await harness();
  await t.emit("hr.update", { bpm: 130 });
  await t.emit("hr.update", { bpm: 150 });
  await t.emit("hr.update", { bpm: 155 }); // inside the cooldown
  assert.deepEqual(t.chat.sent, ["Heart rate just hit 150 bpm 💓"]);
  assert.deepEqual(t.logs, [["warn", "overlay is restarting, skipped"]]);
  const shown: any[] = [];
  t.link("dev.sable.overlay", { show: (a) => { shown.push(a); return true; } });
  await t.clock.advance("2m");
  await t.emit("hr.update", { bpm: 150 });
  assert.deepEqual(shown, [{ text: "150 bpm" }]);
});

test("chat commands: capability, cooldown, aliases", async () => {
  const t = await harness();
  t.twitch.users.wolf = { id: "id-wolf", login: "wolf", name: "Wolf" };
  assert.deepEqual(await t.chatCommand("!zap wolf", { user: "fox" }), []); // viewers lack mod.timeout
  assert.deepEqual(await t.chatCommand("!zap wolf", { user: "owl", roles: ["moderator"] }), ["Wolf got zapped"]);
  assert.equal(t.mod.actions[0].user_id, "id-wolf");
  assert.equal(t.chat.messages[0].mode, "announce");
  await t.emit("hr.update", { bpm: 90 });
  assert.deepEqual(await t.chatCommand("!heartrate"), ["90 bpm"]);
  assert.deepEqual(await t.chatCommand("!hr"), []); // global cooldown
  await t.clock.advance("10s");
  assert.match((await t.chatCommand("!hr"))[0], /90 bpm/);
});

test("cli, config, timers, once", async () => {
  const t = await harness({ config: { threshold: 100, redeem: [{ reward: "Ears", param: "Ears" }] } });
  assert.deepEqual(await t.cli("threshold"), { threshold: 100 });
  assert.deepEqual(await t.cli("threshold", { bpm: 120 }), { threshold: 120 });
  await t.emit("twitch.channel.follow", { user: "fox" });
  await t.emit("twitch.channel.follow", { user: "wolf" });
  await t.clock.advance("31m");
  assert.deepEqual(t.chat.sent, ["First follower: fox!", "Hydration check 💧", "Hydration check 💧"]); // once: not wolf
  await t.emit("twitch.channel.redeem", { user: "fox", reward: "Ears" });
  assert.deepEqual(t.osc.sent, [["/avatar/parameters/Ears", true]]);
  await assert.rejects(harness({ config: { threshold: "high" } }), /threshold: expected int/);
});

test("permissions from plugin.toml", async () => {
  const t = await harness();
  await assert.rejects(bot.osc.setParam("Tail", true), (e) => e instanceof PermissionDenied && /send:\/avatar\/parameters\/Tail/.test(e.message));
  await assert.rejects(bot.chat.whisper("x", "hi"), /"whisper" to \[permissions\].chat/);
  await assert.rejects(bot.twitch.clip(), /"clips" to \[permissions\].twitch/);
  t.perms.store = false;
  await assert.rejects(bot.store.get("x"), /store = true/);
});

test("http through the allowlist", async () => {
  const t = await harness();
  await assert.rejects(bot.http.get("https://api.example.com/x"), /"api.example.com:443" to \[capsule\].net/);
  t.net = ["api.example.com:443"];
  t.http.mock("api.example.com", { json: { ok: 1 } });
  const r = await bot.http.post("https://api.example.com/x", { json: { a: 1 } });
  assert.deepEqual([r.status, r.json()], [200, { ok: 1 }]);
  assert.deepEqual(t.http.requests, [["POST", "https://api.example.com/x", '{"a":1}']]);
});

test("describe matches what luna capsule dev expects", () => {
  const d = bot.describe();
  assert.deepEqual(d.events, ["hr.update", "twitch.channel.follow", "twitch.channel.redeem"]);
  assert.deepEqual(d.chat.map((c: any) => c.name), ["hr", "zap"]);
  assert.deepEqual(d.config.threshold, { type: "int", default: 140 });
  assert.equal(d.config.redeem.type, "list");
  assert.deepEqual(d.links, { provides: [], uses: ["dev.sable.overlay:show"] });
});

test("cbor and protobuf round trips", () => {
  const v = { t: "call", id: 7, b: { method: "spend", body: { amount: -3, big: 2 ** 40, f: 1.5, ok: true, n: null, s: "é", a: [1, "x"] } } };
  assert.deepEqual(JSON.parse(JSON.stringify(decode(encode(v)))), v);
  assert.throws(() => decode(Uint8Array.from([0xa1, 0x61, 0x61])), /truncated/);
  assert.throws(() => decode(Uint8Array.from([0x9f])), /indefinite/);
  const m = { action: "timeout", user_id: "42", seconds: 600, simulated: true, on: false };
  assert.deepEqual(proto.decode("ModRequest", proto.encode("ModRequest", m)), { ...m, reason: "", message_id: "" });
  const r = proto.decode("HrReply", proto.encode("HrReply", { bpm: 90, at_ms: 1767268800000, history: [{ at_ms: 1, bpm: 2 }] }));
  assert.equal(r.at_ms, 1767268800000);
  assert.deepEqual(r.history, [{ at_ms: 1, bpm: 2 }]);
  assert.equal(proto.decode("StoreIncrReply", proto.encode("StoreIncrReply", { value: -5 })).value, -5);
});

test("toml subset", () => {
  const t = parseToml('a = 1\n[x.y]\nb = "q\\"" # c\n[[z]]\nc = [1, 2,\n 3]\nd = { e = true }\n[[z]]\nc = []\n');
  assert.deepEqual(t, { a: 1, x: { y: { b: 'q"' } }, z: [{ c: [1, 2, 3], d: { e: true } }, { c: [] }] });
});
