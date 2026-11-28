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

    def test_a_write_made_after_seeing_another_wins_on_a_slow_clock(self):
        fast, slow = Replica("b", Clock(1000.0)), Replica("a", Clock(997.0))
        fast.set("theme", "dark")
        slow.merge(fast.snapshot())
        slow.set("theme", "light")
        fast.merge(slow.snapshot())
        self.assertEqual(fast.get("theme"), "light")

    def test_concurrent_writes_end_the_same_in_either_merge_order(self):
        a, b = Replica("a", Clock(50.0)), Replica("b", Clock(50.0))
        a.set("x", 1)
        b.set("x", 2)
        one, two = Replica("c", Clock(50.0)), Replica("d", Clock(50.0))
        one.merge(a.snapshot())
        one.merge(b.snapshot())
        two.merge(b.snapshot())
        two.merge(a.snapshot())
        self.assertEqual(one.get("x"), two.get("x"))


if __name__ == "__main__":
    unittest.main()
