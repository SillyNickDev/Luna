import { bot, LinkUnavailable } from "@luna/sdk";

const b = bot.useConfig({ threshold: 140, message: "Heart rate just hit {bpm} bpm 💓", redeem: [] as { reward: string; param: string }[] });
const overlay = bot.links.use("dev.sable.overlay", "show");

bot.on("hr.update", async (ev) => {
  if (await bot.store.get("cooldown")) return;
  await bot.chat.send(b.config.message.replace("{bpm}", String(ev.bpm)));
  await bot.store.set("cooldown", 1, { ttl: "2m" });
  try {
    await overlay.show({ text: `${ev.bpm} bpm` });
  } catch (e) {
    if (!(e instanceof LinkUnavailable)) throw e;
    await bot.log.warn("overlay is restarting, skipped");
  }
}, { when: (ev) => ev.bpm > b.config.threshold });

bot.on("twitch.channel.redeem", async (ev) => {
  for (const m of b.config.redeem) {
    if (m.reward === ev.reward) await bot.osc.setParam(m.param, !(await bot.osc.param(m.param)));
  }
});

bot.once("twitch.channel.follow", (ev) => bot.chat.send(`First follower: ${ev.user!.name}!`));

bot.chatCommand({ name: "hr", aliases: ["heartrate"], cooldown: "10s" }, async (ctx) => {
  await ctx.reply(`${await bot.hr.bpm()} bpm`);
});

bot.chatCommand({ name: "zap", capability: "mod.timeout" }, async (ctx, [target]) => {
  const u = await bot.twitch.user(target ?? "");
  if (!u) return ctx.reply(`no user ${target}`);
  await bot.mod.timeout(u.id, 600, "zapped");
  await bot.chat.announce(`${u.name} got zapped`);
});

bot.cli({ name: "threshold", summary: "get or set the alert threshold", args: [{ name: "bpm", type: "int", optional: true }], mutates: true },
  async ({ bpm }) => {
    if (bpm !== undefined) await bot.store.set("threshold", bpm);
    return { threshold: await bot.store.get("threshold", b.config.threshold) };
  });

bot.timers.every("15m", () => bot.chat.send("Hydration check 💧"));

bot.run();
