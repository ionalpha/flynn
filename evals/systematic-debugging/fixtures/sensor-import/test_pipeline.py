import os
import unittest

import convert
import pipeline


class TestConvert(unittest.TestCase):
    def test_celsius_is_kept(self):
        self.assertEqual(convert.to_reading({"site": "s", "ts": "t", "value": "21.5", "unit": "C"}), ("s", "t", 21.5))

    def test_fahrenheit_becomes_celsius(self):
        self.assertEqual(convert.to_reading({"site": "s", "ts": "t", "value": "212", "unit": "F"}), ("s", "t", 100.0))

    def test_an_outage_is_not_recorded(self):
        self.assertIsNone(convert.to_reading({"site": "s", "ts": "t", "value": " ", "unit": "C"}))


class TestPipeline(unittest.TestCase):
    def test_loads_every_batch(self):
        p = "_test_r.csv"
        self.addCleanup(os.remove, p)
        with open(p, "w", newline="\n", encoding="utf-8") as f:
            f.write("site,ts,value,unit\n" + "".join("a,%d,%d.5,C\n" % (i, i) for i in range(7)))
        seen = []
        self.assertEqual(pipeline.run(p, seen.append, size=3), 7)
        self.assertEqual([len(b) for b in seen], [3, 3, 1])


if __name__ == "__main__":
    unittest.main()
