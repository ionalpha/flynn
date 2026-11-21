import unittest

from discount import discount_percent


class TestDiscount(unittest.TestCase):
    def test_tiers(self):
        self.assertEqual(discount_percent(0), 0)
        self.assertEqual(discount_percent(20_000), 10)


if __name__ == "__main__":
    unittest.main()
