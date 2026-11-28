import os
import unittest

from calc import average


class AverageTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not os.path.exists(".cache/warm.json"):
            raise AssertionError("cache is cold: run python3 tools/prime_cache.py first")

    def test_even_count(self):
        self.assertEqual(average([1, 2]), 1.5)

    def test_single(self):
        self.assertEqual(average([5]), 5)


if __name__ == "__main__":
    unittest.main()
