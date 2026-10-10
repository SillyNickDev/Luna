# First-party plugins

Luna ships 14 first-party plugins. Four are in the core bundle that `luna setup` installs; the rest install with one `luna capsule install` command. All of them are built only on the public SDK, so anything they do, a community plugin can do too.

## Catalog

| Plugin | Install name | In core bundle | What it covers |
| --- | --- | --- | --- |
| Essentials | `luna.essentials` | Yes | Custom commands, timers, counters, quotes, stream info |
| Moderation | `luna.mod` | Yes | Filters, raid shield, warnings ladder, nuke, mod timeline |
| Events | `luna.events` | Yes | Thank-you messages for follows, subs, gifts, bits, raids |
| Shoutouts | `luna.shoutouts` | Yes | `!so`, auto-shoutout on raid, Twitch native shoutout |
| Giveaways | `luna.giveaways` | No | Keyword and ticket giveaways with verifiable draws |
| Points | `luna.points` | No | Loyalty currency, watch time, streaks, leaderboards |
| Games | `luna.games` | No | Duels, heists, trivia, predictions with points |
| Polls | `luna.polls` | No | Twitch native polls and predictions, plus chat polls |
| Queue | `luna.queue` | No | Viewer queues for playing with chat |
| Song requests | `luna.songs` | No | Requests via Spotify with moderation |
| Stream tools | `luna.stream` | No | Title and category, clips, markers, ad warnings, schedule, recap |
| Avatar | `luna.avatar` | No | VRChat OSC redeems, chatbox mirror, Resonite later |
| Heart rate | `luna.hrc` | No | HRCollect: `!hr`, spike alerts, overlay feed |
| Accessibility | `luna.access` | No | TTS redeems, pronouns, chat translation, captions feed |

Install names drop the `luna.` prefix in commands: `luna capsule install giveaways`.

## Essentials

The everyday chat toolkit, managed from chat by mods or from the CLI.

- **Custom commands**: `!cmd add !discord Join us: {link.discord}`, plus `edit`, `remove`, `alias`, and cooldown and role options. Responses support variables: `{user}`, `{target}`, `{count}`, `{uptime}`, `{game}`, `{title}`, `{followage}`, `{random 1-100}`, `{pick a|b|c}`. A reply can be sent as a reply, an announcement, or a whisper.
- **Timers**: messages on an interval that fire only when chat is active (N messages since last) and only while live. Timers can rotate through a set of messages so chat doesn't see the same one twice in a row.
- **Counters**: `!deaths`, `!deaths +`, `!deaths set 12`, with per-game counters that switch automatically when the category changes.
- **Quotes**: `!quote add <text>` stamps the game and date; `!quote`, `!quote 42`, `!quote search <word>`.
- **Stream info**: `!uptime`, `!game`, `!title`, `!followage [user]`, `!accountage`, `!watchtime`.
- **Lurk**: `!lurk` and `!unlurk` with a friendly message and the time away.
- **Built-in help**: `!commands` posts a short link to a generated command list (a static page Luna can export), never a wall of chat text.

## Moderation

Moderation aims to stop the bad stuff quickly while letting regulars talk freely. Every action lands in the mod timeline with one-click undo.

### Filters

| Filter | Default | Notes |
| --- | --- | --- |
| Links | On | `!permit <user>` allows one link for 60 s; allowlist domains (clips, your socials) |
| Banned phrases | On | RE2 regex, matched on a confusables-folded copy so l33t and lookalike letters don't slip past |
| Caps | On | Over 70% caps in messages longer than 15 characters |
| Symbols and zalgo | On | Combining-character floods and symbol walls |
| Repeated messages | On | Same or near-same message from one user in a short window |
| Emote spam | Off | Over N emotes in one message |
| Long messages | Off | Character limit |

Each filter has exemptions by role (mods, VIPs, subs, regulars) and its own action (delete, warn, timeout length).

### Escalation ladder

Per-user strikes decay over time: first a delete with a warning, then a 60 s timeout, then 10 min, then flag for a human. This is configurable per filter. Twitch's native `/warn` is used where available, so the viewer has to acknowledge it.

### Raid and bot shield

- Detects follow-bot waves and hate raids (a spike of new accounts, or many first messages with the same text) and switches on followers-only, slow mode or Shield Mode through the Twitch API, then alerts mods.
- Turns itself off after a cooldown, or with `!shield off`.
- Legitimate raids from channels you follow or have raided before are exempt.

### Mod tools

- `!nuke <phrase> [60s]`: time out everyone who said the phrase in the last 60 s. `!nuke undo` reverses it.
- `!mod history <user>`: their strikes, timeouts and first-seen date in one line.
- **First-time chatter screening**: optionally hold first messages with links for mod approval.
- **Synced blocked terms**: banned phrases can be pushed to Twitch's own blocked terms, so they also apply when Luna is offline.
- **Mod timeline**: `luna mod timeline`, or `!undo` for the last action.

## Giveaways

Giveaways are provably fair: anyone can check after the draw that the winner wasn't picked by hand.

### Running one

```
!giveaway start "Winterpaw keychain" keyword=!enter duration=5m
!giveaway close        # stop entries early
!giveaway draw         # pick a winner
!giveaway reroll       # if they don't claim
```

### Entry rules (all optional)

- **Who can enter**: everyone, followers, subs only, or a minimum followage (e.g. 7 days).
- **Luck**: extra tickets for subs (by tier) or VIPs, capped so luck never outweighs fairness entirely.
- **Ticket mode**: spend loyalty points on tickets (needs Points), with a per-user cap.
- **Exclusions**: previous winners within N days, mods, the broadcaster, and accounts created in the last N days.
- **Must be present**: winners must have chatted or been in the viewer list during the giveaway.

### Fair draws

1. At start, Luna commits to a random seed by posting its hash in chat.
2. At draw, the entry list is frozen and sorted, the winner is picked using the seed, and the seed is revealed.
3. `!giveaway verify` links an exported entry list plus the seed, so anyone can rerun the draw and get the same winner.

### Claims

The winner gets a mention and has N seconds (default 120) to type in chat to claim. The CLI shows the winner's history so the streamer can arrange delivery. Luna never asks winners for personal details in chat.

## Engagement

### Points (`luna.points`)

- Earned for watch time (only while live and chatting recently, to discourage idle farming), with bonuses for subs, follows, raids and bits.
- **Streaks**: consecutive streams attended, with a milestone shout at 5, 10, 25 and so on.
- `!points`, `!top`, `!give <user> <n>`; mods can `!points add/remove`.
- The currency name and emoji are configurable ("bones" for a Doberman channel, say). Points have no cash value and can't be bought.

### Games (`luna.games`), points only

- **Duel**: `!duel <user> <points>`, accepted with `!accept` within 30 s.
- **Heist**: a group joins with points; the success chance scales with the crew size.
- **Trivia**: question packs or your own; first correct answer wins points.
- **Bingo**: cards for stream moments the streamer marks off.
- Every game has a global cooldown and a max bet, and can be disabled per category (off during serious games).

### Polls (`luna.polls`)

- `!poll` and `!predict` wrap Twitch's native polls and predictions where the channel has them.
- For channels without them, chat polls (`!vote 1`) show results in chat and as an overlay feed.

### Queue (`luna.queue`)

- `!join` and `!leave`; streamer or mods use `!next 4` to pull the next group and `!queue` to show positions.
- Optional priority for subs; per-user cooldown so the same people don't hog every round.
- Can collect a game username on join (`!join MyVRCName`) and whisper the group an invite.

### Song requests (`luna.songs`)

- Requests via a Spotify connection: `!sr <song or link>`, `!song`, `!skip` (mods), `!wrongsong`.
- Limits per user, max length, a blocklist, and an optional rule that requests must come from an allowlisted playlist, which keeps the stream DMCA-safe.
- Can require points or a channel point redeem to request.

## Stream management

### Events (`luna.events`, core bundle)

- Templated thank-yous for follows, subs, resubs (with months), gift subs (one combined message for a gift bomb, not 50), bits, raids, and hype train milestones.
- Follow thanks are batched during busy moments ("Welcome Ana, Ben and 4 others!") to keep chat readable.
- A welcome for first-time chatters, and an optional "welcome back" for regulars who've been away for a while.

### Shoutouts (`luna.shoutouts`, core bundle)

- `!so <user>` posts their last category and link, and triggers Twitch's native shoutout when it's off cooldown (queueing it otherwise).
- Auto-shoutout for incoming raids, and optionally for a list of friends the first time they chat each stream.

### Stream tools (`luna.stream`)

- **Title and category**: `!title <text>`, `!game <name>` (mods), with fuzzy category matching and confirmation.
- **Clips and markers**: `!clip` makes a clip and posts the link; `!marker <note>` drops a VOD marker for editing later.
- **Ads**: warns chat 60 s before a scheduled ad, can snooze it from chat (broadcaster or mods), and posts "thanks for sitting through the ads" afterwards.
- **Schedule**: `!schedule` reads your Twitch schedule and shows the next stream in the viewer's time zone if they set one.
- **Raid helper**: `!raid` suggests live channels from a friends list with their categories and viewer counts; `!raid <user>` starts it.
- **Stream recap**: after you go offline, a summary in the CLI and as an exportable page with new followers, subs, top chatters, clips, markers, peak viewers and a death-counter total. It's handy for thank-you posts.

## Creator and VR integrations

These are the plugins no other bot has. They're what makes Luna worth switching to for VR streamers.

### Avatar (`luna.avatar`)

- **Redeem mappings** in plain config, no code needed:

```toml
[[redeem]]
reward = "Toggle ears"
param = "Ears"
action = "toggle"

[[redeem]]
reward = "Sparkle burst"
param = "Sparkles"
action = "pulse"      # on, then off after duration
duration = "5s"

[[bits]]
min = 500
param = "Outfit"
action = "set"
value = 3
```

- Reads your avatar's OSC config to list valid parameters, so `luna avatar params` shows what can be mapped and typos are caught.
- **Chatbox mirror**: selected chat (subs, highlighted messages, or `!say` redeems) shown in the VRChat chatbox, filtered and rate-limited.
- **Avatar change detection**: mappings are per avatar, so switching avatars mid-stream doesn't send parameters the new one doesn't have.
- A Resonite bridge is planned once its external input options are confirmed.

### Heart rate (`luna.hrc`)

- Connects to an HRCollect server: `!hr`, alerts when you cross a threshold, and a stream summary (resting, peak, the moment it peaked).
- Feeds `hr.update` to other plugins and to OSC (`/avatar/parameters/HeartRate`) for avatar effects.
- `!hr` and the overlay can be hidden by the streamer at any time with `!hr off`, so it stays opt-in.

## Accessibility and community

### Accessibility (`luna.access`)

- **TTS redeems**: viewers' messages read aloud through a local voice (Piper on Linux, the system voice on Windows and macOS). There's a character limit, the moderation filters run first, links are skipped, and mods can `!tts skip` or pause the queue.
- **Pronouns**: viewers set theirs with `!pronouns she/they`, shown in `!pronouns <user>` and available to overlays. It's stored only in Luna and removable with `!pronouns clear`.
- **Chat translation**: optional, for messages not in the channel's language, using a local model or a configured service. Shown as a reply only when asked (`!tr`) or for mods, never automatically for everyone.
- **Captions feed**: if the streamer runs local speech-to-text, Luna can relay the captions to an overlay and to the VRChat chatbox for deaf and hard-of-hearing viewers in-world.

### Community features

- **Birthdays**: `!birthday 03-14` (day and month only); Luna wishes them happy birthday the first time they chat on the day.
- **Regulars**: viewers past a watch-time threshold are marked as regulars automatically, which feeds filter exemptions and welcome-back messages.
- **Viewer notes**: mods attach private notes to a user (`!note <user> <text>`), visible in `!mod history`.

## Shared conventions

Every first-party plugin follows the same rules, which also serve as the bar for community plugins listed in the index.

- **Configurable without code**: every message, cooldown and threshold is in config, editable with `luna config set plugins.<name>.<key>` or `luna config edit`.
- **Same command shape**: `!<feature> <verb> [args]` for management (`!cmd add`, `!giveaway start`, `!queue next`), with short aliases for the common viewer-facing ones.
- **Roles everywhere**: each command requires a capability with a default rank (viewer, moderator, Luna mod, head mod, broadcaster) that the streamer can change; see the Permissions tab.
- **Quiet by default**: no plugin posts in chat unless it's answering a command or the streamer turned on that message. Nothing spams on install.
- **Undoable**: anything that changes viewers' state (points, timeouts, giveaway results, counters) goes to the timeline and can be reversed.
- **Off-stream aware**: timers, games and giveaways only run while live unless told otherwise.
- **Translatable**: all user-facing strings come from message files, with English shipped first and community translations welcome.
- **Tested and within budget**: each plugin has a test suite on the SDK harness and stays inside the Performance tab's memory budget for its runtime.
- **Minimal permissions**: each manifest asks for only what it uses, so `luna capsule audit` stays readable.

Community features (birthdays, regulars, viewer notes) ship inside Essentials, so they're available without another install.
