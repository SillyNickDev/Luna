"""Luna plugin SDK for Python.

    from luna import bot

    @bot.on("twitch.channel.follow")
    def welcome(ev):
        bot.chat.send(f"Welcome to the pack, {ev.user.name}!")

    bot.run()
"""

from ._bot import Config, Ctx, Event, bot
from .errors import (ConfigError, LinkUnavailable, LunaError, NotConnected, PermissionDenied, RateLimited,
                     Timeout)

__all__ = ["bot", "Config", "Ctx", "Event", "LunaError", "PermissionDenied", "RateLimited", "LinkUnavailable",
           "ConfigError", "NotConnected", "Timeout"]
