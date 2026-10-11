from luna import Config, bot


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


bot.run()
