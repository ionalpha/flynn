import unittest

import tally


class Summarise(unittest.TestCase):
    def test_counts_by_canonical_host(self):
        lines = [
            "2024-01-01 GET Example.com. 200",
            "2024-01-02 GET example.com 200",
            "2024-01-03 GET other.org 404",
            "not a log line",
        ]
        self.assertEqual(tally.summarise(lines), {"example.com": 2, "other.org": 1})


if __name__ == "__main__":
    unittest.main()
