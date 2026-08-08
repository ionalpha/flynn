import unittest

from discount import discount_percent, total_cents


class TestDiscount(unittest.TestCase):
    def test_tiers(self):
        self.assertEqual(discount_percent(0), 0)
        self.assertEqual(discount_percent(19_999), 0)
        self.assertEqual(discount_percent(20_000), 10)
        self.assertEqual(discount_percent(49_999), 10)
        self.assertEqual(discount_percent(50_000), 20)

    def test_total(self):
        self.assertEqual(total_cents(50_000), 40_000)


if __name__ == "__main__":
    unittest.main()
