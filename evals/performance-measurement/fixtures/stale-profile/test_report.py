import unittest

import report


class Build(unittest.TestCase):
    def test_repeats_are_dropped_and_order_is_kept(self):
        rows = [
            {"account": "b", "day": "2024-03-02", "amount": "5"},
            {"account": "a", "day": "2024-03-01", "amount": "9"},
            {"account": "b", "day": "2024-03-02", "amount": "5"},
            {"account": "a", "day": "2024-03-01", "amount": "10"},
        ]
        lines = report.build(rows)
        self.assertEqual([l.split()[0] for l in lines], ["b", "a", "a"])
        self.assertEqual(lines[2].split()[2], "10")


if __name__ == "__main__":
    unittest.main()
