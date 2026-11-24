# NotebookLM: Luna audio tutorial and deep dive

How to make a NotebookLM Audio Overview that walks users and developers through all of
Luna, from installing it to writing a sandboxed plugin.

## 1. Sources to upload

Upload these files as sources (22 files, about 35 000 words; fits a free notebook):

- **What's built (trust these first):** `README.md`, every file in `Docs/` (except this
  one), `SECURITY-REVIEW.md`, `CONFORMANCE.md`
- **Design intent:** every file in `Specs/`

The Specs were written before the code. Where they disagree with Docs, Docs and
SECURITY-REVIEW.md describe what actually ships; the prompt below says so.

## 2. Prompt (Customize → "What should the AI hosts focus on?")

Use the **Deep Dive** format, **Longer** length. Paste the full prompt; if the box
refuses it as too long, paste the short one from section 3.

```text
Make a start-to-finish tutorial and deep dive on Luna for two listeners at once: a
Twitch streamer who wants to run it, and a developer who wants to write plugins or audit
it. Explain every term the first time it appears, and use a concrete running example: a
streamer in VRChat whose bot greets followers, runs a giveaway, takes song requests and
toggles an avatar outfit when someone redeems channel points.

Where the Specs and the Docs disagree, the Docs, README and SECURITY-REVIEW describe what
is actually built; the Specs are earlier design intent. Say which things are not done
yet (macOS sandbox, Windows backend not yet tested on real hardware, Rust SDK without
http or a test harness) instead of presenting them as finished.

Cover, in this order:

1. What Luna is and why it exists: one self-hosted static Go binary on the streamer's own
   PC instead of a cloud bot, so it can reach VRChat over OSC, a heart-rate monitor and
   local files, and keeps the streamer's credentials local.
2. Getting started as a streamer: setup, logging in the broadcaster and an optional bot
   account, the config file, running the daemon, the terminal dashboard (luna tui, which
   takes mouse clicks), and the core bundle: essentials, moderation, events, shoutouts.
3. The architecture: the event bus, how a Twitch chat message or EventSub event becomes
   an event, the command router (ranks, capabilities, cooldowns), and the output guard
   that cleans everything before it reaches chat.
4. Permissions and ranks: viewer, moderator, luna-mod, head-mod, broadcaster;
   capabilities like polls.run or songs.skip, and how a streamer changes them.
5. The first-party plugins, briefly each: points, giveaways (commit-reveal draws anyone
   can verify), stream tools, games, polls and predictions, the viewer queue, song
   requests through Spotify, accessibility (pronouns, DeepL translation), avatar/OSC and
   heart rate (read by the separate HRCollect app, not by Luna).
6. Capsules, the heart of the design: every plugin runs as its own sandboxed process.
   On Linux: namespaces, pivot_root, Landlock, seccomp, cgroups. On Windows:
   AppContainer and Job Objects. Explain the package format, capsule.lock hashes, the
   install-time permission prompt, and the runtimes (native, luna, python-3.12, node-22).
7. How a plugin talks to Luna: the gRPC plugin API on a Unix socket, checked per call
   against the approved manifest; the CBOR control channel (hello, welcome, ready,
   ping, shutdown); links between plugins through a broker that stamps the caller;
   simulated events that never reach Twitch; the egress proxy that is a plugin's only
   way to the internet, limited to an allowlist.
8. Writing a plugin, step by step: luna capsule init, a five-line plugin in Python,
   TypeScript, Go or Rust, luna capsule dev with simulated events, the in-process test
   harness, pack, install. Compare the SDKs briefly.
9. Distributing plugins: the signed plugin index, install by name, updates, revocation,
   and why entries pin the package's content hash.
10. Security, for the auditor: the assets, the adversaries (viewers, a hostile plugin,
    the network), the 14 trust boundaries, the fuzzers, what the audit prep found and
    fixed, and the known limits listed in SECURITY-REVIEW.md.
11. Performance budgets: memory and startup targets and how one binary and shared
    runtimes keep Luna light next to a VR game.

End with a recap that names the three or four ideas that hold the whole system
together, and where a listener should go next: a streamer to the CLI reference and
configuration reference, a developer to sdk-development.md and the Capsules spec.
Keep the tone friendly and concrete; prefer an example over a list.
```

## 3. Short prompt (if the box has a character limit)

```text
Start-to-finish tutorial and deep dive on Luna for streamers and plugin developers.
Order: what it is and why self-hosted; setup and the TUI; event bus, router, ranks and
output guard; the first-party plugins; capsule sandboxing (Linux, Windows); the plugin
API, links, egress allowlist; writing, testing and packing a plugin; the signed plugin
index; security boundaries and known limits. Docs override Specs where they disagree.
Say what isn't finished. Use a VRChat streamer as the running example.
```

## 4. Tips

- Generate one overview per audience if the combined one runs long: run it twice, with
  "for streamers only, skip sections 7–10" and "for developers and auditors, keep
  sections 1–5 to two minutes".
- Interactive mode lets you interrupt the hosts with questions; the sources above are
  enough for them to answer most "how does X work" questions.
- After big changes, re-upload the changed Docs files (NotebookLM doesn't sync them).
