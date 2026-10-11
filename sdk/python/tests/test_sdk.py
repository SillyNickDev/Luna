import os
import sys
import time
import unittest

os.environ["TZ"] = "UTC"  # cron runs in local time
time.tzset()

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path[:0] = [os.path.join(HERE, ".."), os.path.join(HERE, "example")]

from luna import PermissionDenied  # noqa: E402
from luna.testing import harness  # noqa: E402

import main  # noqa: E402  the example plugin


class SDKTest(unittest.TestCase):
    def test_spike_alerts_once_per_cooldown(self):
        t = harness(main, config={"threshold": 140})
        t.link("dev.sable.overlay", show=lambda text: True)
        t.emit("hr.update", bpm=130)  # under the threshold
        t.emit("hr.update", bpm=150)
        t.emit("hr.update", bpm=155)  # inside the cooldown
        self.assertEqual(t.chat.sent, ["Heart rate just hit 150 bpm 💓"])
        t.clock.advance("2m")
        t.emit("hr.update", bpm=150)
        self.assertEqual(len(t.chat.sent), 2)

    def test_link_down_is_handled(self):
        t = harness(main)
        t.link_down("dev.sable.overlay")
        t.emit("hr.update", bpm=200)
        self.assertIn(("warn", "overlay is restarting, skipped"), t.logs)

    def test_config_from_luna(self):
        t = harness(main, config={"threshold": 180})
        t.emit("hr.update", bpm=170)
        self.assertEqual(t.chat.sent, [])

    def test_filters_once_and_store(self):
        t = harness(main)
        t.emit("twitch.channel.redeem", user="fox", reward="Something else")
        t.emit("twitch.channel.redeem", user="fox", reward="Toggle ears")
        t.emit("twitch.channel.follow", user="fox")
        t.emit("twitch.channel.follow", user="wolf")
        self.assertEqual(t.chat.sent, ["First follower: fox!"])
        self.assertEqual(t.chat_command("!hr").replies, ["1 ear toggles so far"])

    def test_cli_command(self):
        t = harness(main)
        self.assertEqual(t.cli("threshold"), {"threshold": 140})
        self.assertEqual(t.cli("threshold", "160"), {"threshold": 160})
        self.assertEqual(t.cli("threshold", reset=True), {"threshold": 140})

    def test_chat_command_args_and_cooldown(self):
        t = harness(main)
        self.assertEqual(t.chat_command("!duel wolf 5", user="fox").replies, ["fox challenges wolf to 5 rounds"])
        self.assertEqual(t.chat_command("!duel", user="fox").replies, ["usage: !duel <target> <rounds>"])
        self.assertEqual(len(t.chat_command("!heartrate").replies), 1)
        self.assertEqual(t.chat_command("!hr").replies, [])  # global 10s cooldown
        t.clock.advance("10s")
        self.assertEqual(len(t.chat_command("!hr").replies), 1)

    def test_timers_on_fake_clock(self):
        t = harness(main)
        t.clock.advance("31m")
        self.assertEqual(t.chat.sent, ["Hydration check 💧"] * 2)

    def test_describe(self):
        d = main.bot.describe()
        self.assertEqual(d["events"], ["hr.update", "twitch.channel.follow", "twitch.channel.redeem", "twitch.chat.message"])
        self.assertEqual(d["config"], {"threshold": {"type": "int", "default": 140}})
        cli = d["cli"][0]
        self.assertEqual((cli["args"], cli["flags"]), ([{"name": "bpm", "type": "int", "optional": True}],
                                                       [{"name": "reset", "type": "bool", "optional": True}]))
        self.assertEqual(d["links"]["uses"], ["dev.sable.overlay:show"])

    def test_permissions_enforced(self):
        t = harness(main)
        t.perms = {"events": ["hr.update", "twitch.channel.*"]}  # as if chat and store were missing
        with self.assertRaises(PermissionDenied) as e:
            main.bot.chat.send("hi")
        self.assertIn('add "send" to [permissions].chat', str(e.exception))

    def test_wait_for(self):
        t = harness(main)
        self.assertEqual(t.chat_command("!fight wolf", user="fox").replies, ["wolf, type !accept within 30s"])
        t.chat_command("!accept", user="owl")  # not the one challenged
        self.assertEqual(t.chat_command("!accept", user="wolf").replies, ["Fight!"])
        t.chat_command("!fight bear", user="fox")
        t.clock.advance("31s")
        self.assertEqual(t.chat.sent[-1], "bear chickened out")

    def test_mod_twitch_viewers(self):
        t = harness(main)
        t.twitch.users["wolf"] = {"id": "id-wolf", "login": "wolf", "name": "Wolf"}
        self.assertEqual(t.chat_command("!zap wolf", user="fox").replies, [])  # viewers lack mod.timeout
        self.assertEqual(t.chat_command("!zap wolf", user="owl", roles=["moderator"]).replies, ["Wolf got zapped"])
        self.assertEqual(t.mod.actions, [("timeout", "id-wolf", 600)])
        self.assertEqual(t.chat.messages[-1]["mode"], "announce")
        self.assertEqual(t.chat_command("!zap ghost", user="owl", roles=["moderator"]).replies, ["no user ghost"])
        t.viewers["id-fox"] = {"id": "id-fox", "login": "fox", "messages": 12}
        self.assertEqual(t.chat_command("!seen fox").replies, ["fox: 12 messages"])
        reply = t.chat.messages[-1]
        self.assertTrue(reply["reply_to"].startswith("msg-"), reply)

    def test_twitch_writes(self):
        t = harness(main)
        t.twitch.categories = [{"id": "3", "name": "VRChat"}]
        self.assertEqual(main.bot.twitch.categories("vr")[0].name, "VRChat")
        with self.assertRaises(PermissionDenied) as e:
            main.bot.twitch.clip()  # the example only asks for read
        self.assertIn('"clips" to [permissions].twitch', str(e.exception))
        t.perms = dict(t.perms, twitch=["read", "edit", "clips"])
        self.assertEqual(main.bot.twitch.clip().url, "https://clips.twitch.tv/clip1")
        main.bot.twitch.set_channel(title="New title")
        self.assertEqual(t.twitch.writes, [("clip", {}), ("set_channel", {"title": "New title", "game_id": ""})])

    def test_osc_hr_and_list_config(self):
        from luna import Config
        t = harness(main)
        t.emit("hr.update", bpm=88)
        t.clock.advance("1m")
        t.emit("hr.update", bpm=91)
        self.assertEqual(main.bot.hr.bpm, 91)
        self.assertEqual([s.bpm for s in main.bot.hr.history(5)], [88, 91])
        with self.assertRaises(PermissionDenied):
            main.bot.osc.set_param("Ears", True)  # the example asks for no osc
        t.perms = dict(t.perms, osc=["send:/avatar/parameters/E*", "read"])
        main.bot.osc.set_param("Ears", True)
        self.assertEqual(t.osc.sent, [("/avatar/parameters/Ears", True)])
        t.osc.params["Ears"] = True
        self.assertIs(main.bot.osc.param("Ears"), True)
        with self.assertRaises(PermissionDenied):
            main.bot.osc.chatbox("hi")

        class S(Config):
            redeem: list = []
        cfg = S._load({"redeem": [{"reward": "Toggle ears", "param": "Ears"}]})
        self.assertEqual(cfg.redeem[0].param, "Ears")
        self.assertEqual(S._schema()["redeem"]["type"], "list")

    def test_http(self):
        t = harness(main)
        with self.assertRaises(PermissionDenied) as e:
            main.bot.http.get("https://api.example.com/x")  # the example lists no hosts
        self.assertIn('"api.example.com:443" to [capsule].net', str(e.exception))
        t.net = ["api.example.com:443"]
        t.http.mock("api.example.com", json={"ok": 1})
        r = main.bot.http.post("https://api.example.com/x", json={"a": 1})
        self.assertEqual((r.status, r.json()), (200, {"ok": 1}))
        self.assertEqual(t.http.requests, [("POST", "https://api.example.com/x", b'{"a": 1}')])

    def test_mod_permission(self):
        t = harness(main)
        t.perms = dict(t.perms, mod=[])
        with self.assertRaises(PermissionDenied) as e:
            main.bot.mod.timeout("id-x")
        self.assertIn('"timeout" to [permissions].mod', str(e.exception))

    def test_store_model_and_transaction(self):
        t = harness(main)
        t.store["streak_broken"] = (b"true", 0)
        self.assertEqual(t.cli("win"), {"wins": 1})
        self.assertEqual(t.cli("win"), {"wins": 2})
        self.assertNotIn("streak_broken", t.store)
        self.assertEqual(t.store["last"][0], b'"win"')

    def test_cron_and_chat_activity(self):
        t = harness(main)
        t.clock.advance("33h")  # Thursday 12:00 UTC to Friday 21:00
        self.assertEqual(t.chat.sent.count("Friday stream tonight!"), 1)
        self.assertNotIn("Chat is lively", t.chat.sent)  # nobody chatted
        t.chat_command("hello all", user="fox")
        t.clock.advance("10m")
        self.assertEqual(t.chat.sent.count("Chat is lively"), 1)

    def test_cron_parse(self):
        from luna._bot import _cron, _cron_due
        c = _cron("*/15 9-17 * * MON-FRI")
        self.assertTrue(_cron_due(c, time.gmtime(1767355200)))  # Fri 2026-01-02 12:00
        self.assertFalse(_cron_due(c, time.gmtime(1767441600)))  # Sat
        with self.assertRaises(ValueError):
            _cron("61 * * * *")


if __name__ == "__main__":
    unittest.main()
