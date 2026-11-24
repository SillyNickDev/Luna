# Configuration Reference (`luna.toml`)

Luna configurations are written in standard [TOML](https://toml.io/) format and stored in a single unified configuration file: `luna.toml`.

The configuration system supports **live hot-reloading**. When `luna.toml` is modified, Luna validates the new schema. If valid, the new configuration is applied atomically without dropping the Twitch connection. If invalid, Luna logs the error and continues running with the last known good configuration.

---

## 1. File Locations & File Permissions

### Default File Paths
- **Linux / macOS**: `~/.config/luna/luna.toml`
- **Windows**: `%APPDATA%\luna\luna.toml`
- **Data Directory**: `~/.local/share/luna/` (contains SQLite database, capsule store, and secrets).

### Isolated Multi-Channel Configuration
To run a separate instance for a second Twitch channel or testing environment, pass the `--config` flag:
```sh
luna --config ~/luna-test daemon
```
This isolates the configuration file (`~/luna-test/luna.toml`), data directory (`~/luna-test/data/`), SQLite database, and control socket.

> [!CAUTION]
> On Linux and macOS, Luna strictly validates file permissions. If `luna.toml` is writable by other users (`group` or `others`), Luna refuses to start to prevent local privilege escalation. Fix with:
> ```sh
> chmod go-w ~/.config/luna/luna.toml
> ```

---

## 2. Complete Annotated `luna.toml` Example

```toml
# ==============================================================================
# Luna Core Configuration (luna.toml)
# ==============================================================================

# Target soft memory ceiling for the Go runtime (in MiB).
# Sets GOMEMLIMIT, optimizing garbage collection under memory pressure.
memory_limit_mib = 48

# ------------------------------------------------------------------------------
# Twitch Integration Settings
# ------------------------------------------------------------------------------
[twitch]
# Luna's default public Twitch client ID. Override only if registering your own app.
client_id = "9ye8kbj2uo87wxll4l9qsuoowt7yub"

# ------------------------------------------------------------------------------
# Chat & Rate Limiting Settings
# ------------------------------------------------------------------------------
[chat]
# Maximum outbound chat messages permitted per 30-second token bucket window (1-100).
rate_limit = 20

# Global RE2 regular expressions for banned words/phrases (case-insensitive).
# Any outgoing message matching these patterns is blocked by the Output Guard.
blocklist = [
  "badphrase",
  "scamwebsite\\.com"
]

# ------------------------------------------------------------------------------
# VRChat OSC Integration
# ------------------------------------------------------------------------------
[osc]
# Enable VRChat OSC integration (UDP)
enabled = false

# Outbound UDP target address (VRChat OSC input)
send = "127.0.0.1:9000"

# Inbound UDP listening address (VRChat OSC output)
listen = "127.0.0.1:9001"

# Directory containing VRChat avatar OSC JSON configuration files.
# If empty, automatically detected under Steam Proton (Linux) or LocalLow (Windows).
config_dir = ""

# Require explicit opt-in if binding OSC across a non-loopback network interface,
# as OSC is unencrypted UDP with no authentication.
allow_remote = false

# ==============================================================================
# Plugin Settings ([plugins.<name>])
# ==============================================================================

# ------------------------------------------------------------------------------
# Core Bundle: Essentials, Moderation, Events, Shoutouts (luna.core)
# ------------------------------------------------------------------------------
[index]
url = ""          # where index.json and the .capsule files are (https)
public_key = ""   # the index maintainer's Ed25519 public key (luna index keygen prints it)

[spotify]
client_id = ""   # from your app at developer.spotify.com (redirect URI http://127.0.0.1:43219/callback)

[plugins.core]
# Essentials
commands_url       = "https://example.com/commands"  # URL posted by !commands
lurk_message       = "{user} is lurking in the shadows. Enjoy the stream!"
unlurk_message     = "Welcome back, {user}! You were away for {away}."
timer_min_messages = 5                               # Chat lines required between timers
regular_watch_hours= 10                              # Hours required to be recognized as regular
birthday_message   = "Happy birthday, {user}! 🎂"

# Moderation Filters & Strike Ladder
mod_exempt         = "vip"                           # Exempt roles: "vip", "sub", or "none"
mod_ladder         = "delete,60s,10m,flag"           # Progressive strike disciplinary actions
mod_strike_decay_m = 60                              # Minutes before strikes decay
mod_warn_native    = true                            # Use Twitch native /warn dialogs
mod_links          = true                            # Enable link filter
mod_link_allow     = "clips.twitch.tv,youtube.com"   # Allowlisted domains (comma-separated)
mod_permit_s       = 60                              # Duration of !permit in seconds
mod_phrases        = true                            # Enable RE2 banned phrases filter
mod_caps           = true                            # Enable caps filter
mod_caps_percent   = 70                              # Caps percentage threshold
mod_caps_min_len   = 15                              # Minimum message length to evaluate caps
mod_symbols        = true                            # Enable symbol / zalgo spam filter
mod_repeat         = true                            # Enable repeated message filter
mod_repeat_count   = 3                               # Repetitions required to trigger
mod_repeat_window_s= 30                              # Window in seconds for repeat filter
mod_emotes         = false                           # Enable emote flood filter
mod_emotes_max     = 10                              # Maximum emotes per message
mod_long           = false                           # Enable long message filter
mod_long_max       = 300                             # Maximum character length
mod_nuke_timeout_s = 600                             # Default timeout for !nuke (10m)

# Raid & Follow-Bot Shield
mod_shield         = true                            # Enable automated raid detection
mod_shield_follows = 20                              # Follow threshold in 30s to trigger shield
mod_shield_same_text= 8                              # Duplicate message count to trigger shield
mod_shield_window_s= 30                              # Detection evaluation window
mod_shield_off_m   = 10                              # Minutes before auto-deactivating shield

# Event Alerts (comma-separated list of enabled alerts)
# Options: follow, sub, resub, gift, bits, raid, first, welcome_back
events_on          = "follow,sub,resub,gift,bits,raid,first,welcome_back"
follow_message     = "Welcome to the pack, {names}!"
follow_batch_s     = 10                              # Seconds to batch incoming follows
sub_message        = "{user} just subscribed! Thank you 💜"
resub_message      = "{user} resubscribed for {months} months! Thank you 💜"
gift_message       = "{user} gifted {count} subs to the community! Thank you 🎁"
bits_message       = "{user} cheered {bits} bits! Thank you ✨"
raid_message       = "{user} is raiding with {viewers} viewers! Welcome, raiders!"
first_message      = "Welcome to chat for the first time, {user}! 👋"
welcome_back_message= "Welcome back, {user}! It's great to see you again."
welcome_back_days  = 14                              # Days away to trigger welcome back

# Shoutouts
so_message         = "Go check out {name}! Last playing {game}: https://twitch.tv/{login}"
so_native          = true                            # Trigger native Twitch /shoutout
so_raids           = true                            # Automatically shoutout incoming raiders
so_friends         = "friend_streamer,collab_partner" # Friends to auto-shoutout when they chat

# ------------------------------------------------------------------------------
# Points Plugin (luna.points)
# ------------------------------------------------------------------------------
[plugins.points]
name               = "bones"                         # Currency display name
emoji              = "🦴"                            # Currency emoji
watch_points       = 10                              # Points awarded per watch interval
watch_interval_m   = 5                               # Minutes between watch payouts
active_window_m    = 10                              # Chatter must have chatted in last N min
sub_bonus_percent  = 50                              # Bonus percentage payout for subs
follow_bonus       = 50                              # Bonus points for following
sub_bonus          = 250                             # Bonus points for subscribing
gift_bonus         = 100                             # Bonus points per gifted sub
bits_per_100       = 10                              # Points per 100 bits cheered
raid_bonus         = 100                             # Bonus points for raiders
streak_shout       = true                            # Announce watch streaks in chat
streak_message     = "{user} has attended {streak} streams in a row! 🔥"

# ------------------------------------------------------------------------------
# Giveaways Plugin (luna.giveaways)
# ------------------------------------------------------------------------------
[plugins.giveaways]
live_only          = true                            # Only allow giveaways while live
keyword            = "!enter"                        # Chat entry command
claim_s            = 120                             # Seconds to claim before reroll
exclude_mods       = false                           # Exclude channel moderators from winning
min_account_days   = 7                               # Minimum account age in days to enter
winner_cooldown_days = 14                            # Days a winner must wait before winning again
sub_luck           = 2                               # Extra tickets for subscribers
vip_luck           = 1                               # Extra tickets for VIPs
luck_cap           = 3                               # Maximum luck multiplier allowed
start_message      = "🎁 Giveaway: {prize}! Type {how} to enter! Fair-draw commitment: {commit}…"
winner_message     = "🎉 {user} won {prize}! Type in chat within {claim} to claim. Seed: {seed}"

# ------------------------------------------------------------------------------
# Stream Tools Plugin (luna.stream)
# ------------------------------------------------------------------------------
[plugins.stream]
ad_warn            = true                            # Warn chat before scheduled ad breaks
ad_warn_s          = 60                              # Seconds notice before ad break
ad_warn_message    = "Ad break in {in} for {duration}. Grab some water! 💜"
ad_thanks          = true                            # Post thank you after ads finish
ad_thanks_message  = "Thanks for sitting through the ads and supporting the channel! 💜"
clip_message       = "🎬 Clip created: {url}"
raid_friends       = "streamer_a,streamer_b"         # Priority list for !raid suggestions
schedule_timezone  = "America/New_York"              # Default timezone for !schedule
recap_top_chatters = 5                               # Number of top chatters in offline recap

# ------------------------------------------------------------------------------
# Avatar OSC Integration Plugin (luna.avatar)
# ------------------------------------------------------------------------------
[plugins.avatar]
# Chatbox Mirroring: "off", "subs", "highlighted", or "say"
chatbox            = "subs"
chatbox_format     = "{user}: {text}"

# Channel Point Redemption Mappings to VRChat Avatar Parameters
[[plugins.avatar.redeem]]
reward   = "Toggle Ears"
param    = "Ears"
action   = "toggle"

[[plugins.avatar.redeem]]
reward   = "Sparkle Burst"
param    = "Sparkles"
action   = "pulse"
duration = "5s"

# Bits Cheer Mappings
[[plugins.avatar.bits]]
min      = 500
param    = "Outfit"
action   = "set"
value    = 3

# ------------------------------------------------------------------------------
# Heart Rate Telemetry Plugin (luna.hrc)
# ------------------------------------------------------------------------------
[plugins.hrc]
name               = "Streamer"
alert              = true                            # Enable heart rate spike alerts
alert_bpm          = 140                             # Threshold BPM
alert_message      = "{name}'s heart rate just spiked to {bpm} bpm! 💓"
alert_cooldown_s   = 300                             # Seconds before another alert can fire
osc_param          = "HeartRate"                     # Synchronize to VRChat avatar parameter
stale_s            = 60                              # Seconds before reading is marked stale
summary_chat       = false                           # Announce HR summary in chat when offline
```

---

## 3. Configuration Management Commands

### Inspecting Live Configuration
```sh
luna config show                    # Print effective config (tokens and secrets masked)
luna config show chat.rate_limit    # Inspect a single section or key
```

### Modifying Settings on the Fly
```sh
luna config set chat.rate_limit 25
luna config set plugins.hrc.alert_bpm 150
```

### Interactive Editor Validation
```sh
luna config edit
```
Opens `luna.toml` in your `$EDITOR`. Upon exit, Luna automatically executes validation checks. If any errors are found, Luna provides detailed line and key error descriptions and refuses to replace the live configuration until corrected.
