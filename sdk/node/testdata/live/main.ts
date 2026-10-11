import { bot } from "@luna/sdk";

const b = bot.useConfig({ greeting: "Welcome" });
bot.onStart(() => bot.store.set("started", { at: 1, big: 2 ** 40, neg: -5 }));
bot.on("twitch.channel.follow", async (ev) => {
  const n = await bot.store.incr("follows");
  await bot.chat.send(`${b.config.greeting}, ${ev.user!.name}! #${n}`);
});
bot.cli({ name: "count", summary: "follows seen" }, async () => ({ follows: await bot.store.get("follows", 0) }));
bot.run();
