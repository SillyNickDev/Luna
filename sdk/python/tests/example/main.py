from luna import Config, bot, events


class Settings(Config):
    threshold: int = 140


bot.use_config(Settings)
overlay = bot.links.use("dev.sable.overlay", "show")


@bot.on("hr.update", when=lambda ev: ev.bpm > bot.config.threshold)
def spike(ev):
    if bot.store.get("cooldown"):
        return
    bot.chat.send(f"Heart rate just hit {ev.bpm} bpm 💓")
    bot.store.set("cooldown", 1, ttl="2m")
    try:
        overlay.show(text=f"{ev.bpm} bpm")
    except bot.LinkUnavailable:
        bot.log.warn("overlay is restarting, skipped")


@bot.on("twitch.channel.redeem", reward="Toggle ears")
def ears(ev):
    bot.store.incr("ears")


@bot.once("twitch.channel.follow")
def first_follow(ev):
    bot.chat.send(f"First follower: {ev.user.name}!")


@bot.cli("threshold", mutates=True)
def threshold(bpm: int | None = None, reset: bool = False):
    """Get or set the heart rate alert threshold."""
    if reset:
        bot.store.delete("threshold")
    elif bpm is not None:
        bot.store.set("threshold", bpm)
    return {"threshold": bot.store.get("threshold", default=bot.config.threshold)}


@bot.chat_command("hr", aliases=["heartrate"], cooldown="10s")
def hr(ctx):
    ctx.reply(f"{bot.store.get('ears', 0)} ear toggles so far")


@bot.chat_command("duel")
def duel(ctx, target: str, rounds: int = 3):
    ctx.reply(f"{ctx.user.name} challenges {target} to {rounds} rounds")


@bot.timers.every("15m")
def hydrate():
    bot.chat.send("Hydration check 💧")


bot.listen(events.TWITCH_CHAT_MESSAGE)  # for wait_for below


@bot.chat_command("fight")
async def fight(ctx, target: str):
    ctx.reply(f"{target}, type !accept within 30s")
    ok = await bot.wait_for("twitch.chat.message", user=target, text="!accept", timeout=30)
    ctx.reply("Fight!" if ok else f"{target} chickened out")


@bot.chat_command("zap", capability="mod.timeout")
def zap(ctx, target: str):
    who = bot.twitch.user(target)
    if who is None:
        return ctx.reply(f"no user {target}")
    bot.mod.timeout(who, "10m", reason="zapped")
    bot.chat.announce(f"{who.name} got zapped")


@bot.chat_command("seen")
def seen(ctx, who: str):
    v = bot.viewers.get(who)
    ctx.reply(f"{who}: {v.messages} messages" if v else f"never seen {who}")


class Stats(Config):
    wins: int = 0


@bot.cli("win", mutates=True)
def win():
    """Count a win."""
    stats = bot.store.model("stats", Stats)
    stats.wins += 1
    with bot.store.transaction():
        bot.store.set("last", "win")
        bot.store.delete("streak_broken")
    return {"wins": stats.wins}


@bot.timers.cron("0 20 * * FRI")
def friday():
    bot.chat.send("Friday stream tonight!")


@bot.timers.every("10m", only_if_chat_active=True)
def lively():
    bot.chat.send("Chat is lively")


bot.run()
