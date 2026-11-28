import unittest

from pricing import checkout_total


class TestPricing(unittest.TestCase):
    def test_total(self):
        self.assertEqual(checkout_total([("tea", 2), ("mug", 1)]), 2100)


if __name__ == "__main__":
    unittest.main()
