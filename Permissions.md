# Permissions

## Rank ladder

&#91;embedded content: rank ladder · 5 ranks\]

Luna mod is the one rank Twitch doesn't have; it's how a streamer extends trust to specific mods without making them head mod.

Luna has five ranks: broadcaster, head mod, Luna mod, moderator, viewer. A higher rank can always see and override a lower one; a lower rank can never undo a higher one.

## Ranks

| Rank | Who | How they get it | Can do by default |
| --- | --- | --- | --- |
| **Broadcaster** | The channel owner | Fixed: the account Luna was set up with | Everything, including granting ranks, editing permissions and installing capsules |
| **Head mod** | Twitch Lead Moderators | Detected from the `lead_moderator` badge, or set by the broadcaster (see below) | Manage Luna mods, lock actions, run giveaways, change filters and commands, override any mod |
| **Luna mod** | Twitch mods who signed in to Luna and were approved | Broadcaster or head mod approves after the mod signs in | Moderator powers plus mod timeline undo, `!luna` management commands on the allowlist, and overriding plain moderators |
| **Moderator** | Any Twitch mod in the channel | Automatic from Twitch | Twitch's own mod actions, Luna's mod commands (`!permit`, `!nuke`, `!cmd add`), undo their own actions |
| **Viewer** | Everyone else | Default | Chat, watch, and use viewer commands (`!points`, `!enter`, `!join`, `!hr`). No permissions. |

Subs and VIPs are viewers with **perks**, not permissions: extra giveaway luck, filter exemptions, priority in queues. Perks never unlock management actions.

### Detecting head mods

Twitch's API doesn't say who is a Lead Moderator; only the chat badge does, and a lead mod can choose to show the normal mod badge instead. So Luna keeps its own head mod list:

- When a `lead_moderator` badge appears in chat, that mod becomes head mod and the broadcaster is notified.
- The broadcaster can also set it directly: `luna perms set <user> head-mod`.
- If the badge later disappears, nothing changes automatically, since it may be a badge choice. Only the broadcaster removing them, or Twitch unmodding them, ends it.

## Override rules

Every action Luna records (timeout, ban, deletion, filter change, command edit, points adjustment, giveaway result) stores who did it and their rank at the time. Whether someone can undo or change it follows four rules:

1. **Higher rank** can always override a lower rank's action.
2. **Same rank** can override each other only if the broadcaster turns it on for that rank (`perms.same_rank_override`). Defaults: off for moderators, on for Luna mods and head mods.
3. **Lower rank** can never override a higher rank's action. They get a reply naming who made it, and can `!appeal` to flag it for that person.
4. **Your own actions** you can always undo, unless someone higher has locked them.

### Locks

Head mods and the broadcaster can lock an action so only their rank or above can change it: `!lock ban <user>` or `luna mod lock <action-id>`. This suits decisions made after discussion that shouldn't be quietly reversed.

### What override means in practice

| Situation | Result |
| --- | --- |
| A moderator unbans someone a head mod banned | Refused; the moderator is told it was a head mod's ban |
| A Luna mod undoes a moderator's timeout | Allowed; both appear in the timeline |
| Two moderators disagree on a timeout (same-rank override off) | The second can't undo it; they can extend it or `!appeal` |
| The broadcaster reverses anything | Always allowed |
| An action taken directly in Twitch's own mod tools | Luna logs it with the actor's rank and the same rules apply to undoing it through Luna. Luna can't stop Twitch's native tools |

The last row matters: Luna enforces its rules on actions made through Luna. Twitch's own mod buttons only know Twitch's roles, so a mod could still unban through Twitch. Luna flags those as **bypass** events in the timeline and alerts the original actor.

## Luna mods

A Luna mod is a Twitch mod who has proved their identity to Luna with their own Twitch login and been approved by someone trusted. Signing in also means their mod actions through Luna are made with their own token, so Twitch's own logs show the real person, not the bot.

### Becoming one

1. The mod types `!luna signin` in chat. Luna whispers them a Twitch device-code link (the code is never posted in public chat).
2. They approve on Twitch's own site with moderator scopes only. Luna checks the token's user ID matches the chatter and that they're a current mod.
3. The request lands in the approval queue: `luna perms pending` in the CLI, or `!luna approve <user>` for the broadcaster and head mods.
4. Once approved they're a Luna mod. They're told in a whisper, and it's logged.

No network port is opened for this; everything runs through Twitch chat, whispers and Twitch's own login page.

### Staying one

- **Revocation**: the broadcaster or a head mod runs `luna perms revoke <user>` or `!luna revoke <user>`. Their Luna token is deleted and revoked with Twitch immediately.
- **Twitch removes their mod**: Luna sees the `channel.moderator.remove` event and drops every rank they had, at once.
- **Token problems**: if their token expires or is revoked from their side, they drop to moderator until they sign in again.
- **Inactivity**: optional auto-demotion to moderator after N days without using Luna, so old trust doesn't linger.

Their token is stored like the broadcaster's (Security tab), scoped to moderation only, and never usable for anything else.

## Capabilities and grants

Ranks are bundles of **capabilities**: named permissions such as `mod.timeout` or `giveaway.run`. Each rank has defaults, and the broadcaster can adjust any rank, or any one person, without inventing new ranks.

| Capability | Viewer | Moderator | Luna mod | Head mod | Broadcaster |
| --- | --- | --- | --- | --- | --- |
| `chat.commands.use` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `mod.delete`, `mod.timeout`, `mod.warn` |  | ✓ | ✓ | ✓ | ✓ |
| `mod.ban` |  | ✓ | ✓ | ✓ | ✓ |
| `mod.undo.own` |  | ✓ | ✓ | ✓ | ✓ |
| `mod.undo.others` |  |  | ✓ | ✓ | ✓ |
| `mod.lock` |  |  |  | ✓ | ✓ |
| `commands.edit` |  | ✓ | ✓ | ✓ | ✓ |
| `filters.edit` |  |  | ✓ | ✓ | ✓ |
| `giveaway.run` |  |  | ✓ | ✓ | ✓ |
| `points.adjust` |  |  | ✓ | ✓ | ✓ |
| `stream.edit` (title, category, ads) |  |  | ✓ | ✓ | ✓ |
| `luna.chat_cli` (allowlisted `!luna` commands) |  |  | ✓ | ✓ | ✓ |
| `perms.approve_luna_mods` |  |  |  | ✓ | ✓ |
| `perms.grant`, `capsule.install`, `auth.*`, `config.security` |  |  |  |  | ✓ |

### Commands

| Command | Does |
| --- | --- |
| `luna perms show [<user>]` | Ranks and effective capabilities, with where each came from |
| `luna perms set <user> <rank>` | Assign head mod or Luna mod (broadcaster only for head mod) |
| `luna perms grant <rank\|user> <capability>` / `revoke` | Adjust defaults for a rank or one person |
| `luna perms pending` / `approve` / `deny` | The Luna mod sign-in queue |
| `luna perms check <user> <capability>` | Explain whether they can, and why not if not |
| `luna perms reset` | Back to defaults (asks first) |

`perms.grant` and anything touching auth or security can never be granted below broadcaster, so a compromised mod can't escalate themselves.

## Plugins and permissions

Plugins plug into the same system instead of inventing their own role checks.

- **Commands declare a capability**, not a role list. Luna checks it before the plugin sees the call:

```toml
[[chat.commands]]
name = "giveaway"
capability = "giveaway.run"     # an existing capability

[[chat.commands]]
name = "enter"
capability = "chat.commands.use"

[[capabilities]]
name = "queue.manage"             # a plugin's own, namespaced as luna.queue/queue.manage
default_rank = "luna-mod"
```

- **Plugin-defined capabilities** appear in `luna perms show` alongside built-ins, and the broadcaster can move them between ranks like any other.
- **Plugins can't grant**: no SDK call changes ranks or capabilities. A plugin can ask `ctx.user.can("queue.manage")` and read `ctx.user.rank`, nothing more.
- **Attribution**: when a mod's command makes a plugin take a mod action, the action is recorded as the mod's (with the plugin noted), so override rules apply to the person, not the plugin.
- **Overrides through plugins**: undoing a giveaway result or a points change goes through the same rank check as a ban.

In the SDK, `bot.chat_command("giveaway", capability="giveaway.run")` replaces the older `roles=` option, and `roles=` still works as shorthand for a minimum rank.

## Safety rules and edge cases

- **Identity is the Twitch user ID**, never a display name, so renamed or lookalike accounts can't inherit a rank.
- **Twitch is the floor**: Luna never gives Twitch powers to someone Twitch hasn't. A Luna mod who loses Twitch mod loses everything at once.
- **The broadcaster can't be overridden or demoted** by anyone, and the bot account has no rank of its own. It acts only on behalf of whoever triggered it.
- **Lockdown**: `!luna lockdown` (head mod or broadcaster) freezes every rank below head mod to moderator powers and pauses `!luna` chat commands, for when a mod account looks compromised. `!luna lockdown off` restores it.
- **Audit**: every rank change, grant, approval, override, lock and bypass is in the timeline with who did it. `luna perms history` shows them on their own.
- **Two-person rule (optional)**: perma-bans or bans of subs over N months can require a second Luna mod or above to confirm before Luna sends them.
- **Offline**: rank checks run in the core and need no network, so permissions keep working if Twitch's API is slow. Rank changes from Twitch events apply as soon as they arrive.
