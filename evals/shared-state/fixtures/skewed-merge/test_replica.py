import json
import unittest

from replica import Replica


class Clock:
    def __init__(self, now):
        self.now = now

    def __call__(self):
        return self.now


class Sync(unittest.TestCase):
    def test_a_later_write_wins_after_a_merge(self):
        a, b = Replica("a", Clock(100.0)), Replica("b", Clock(100.0))
        a.set("theme", "dark")
        b.merge(a.snapshot())
        b.clock.now = 105.0
        b.set("theme", "light")
        a.merge(json.loads(json.dumps(b.snapshot())))
        self.assertEqual(a.get("theme"), "light")

    def test_merging_twice_changes_nothing(self):
        a, b = Replica("a", Clock(100.0)), Replica("b", Clock(100.0))
        a.set("language", "en")
        b.merge(a.snapshot())
        b.merge(a.snapshot())
        self.assertEqual(b.get("language"), "en")


if __name__ == "__main__":
    unittest.main()
