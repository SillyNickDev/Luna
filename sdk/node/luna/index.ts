// @luna/sdk: write a Luna plugin in TypeScript or JavaScript.
//
//   import { bot } from "@luna/sdk";
//   bot.on("twitch.channel.follow", (ev) => bot.chat.send(`Welcome to the pack, ${ev.user.name}!`));
//   bot.run();
export {
  bot, Bot, Ctx, rankAtLeast, ms, LunaError, PermissionDenied, RateLimited, NotConnected, Timeout, LinkUnavailable, ConfigError,
} from "./bot.ts";
export type { Event, User, Json, ChatSpec, CliSpec, ArgSpec } from "./bot.ts";
export * as events from "./events.ts";
