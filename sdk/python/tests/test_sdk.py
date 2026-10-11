import os
import sys
import unittest

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
        self.assertEqual(d["events"], ["hr.update", "twitch.channel.follow", "twitch.channel.redeem"])
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


if __name__ == "__main__":
    unittest.main()
