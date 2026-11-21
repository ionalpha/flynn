import unittest

from discount import price_after_discount as price


class PriceAfterDiscount(unittest.TestCase):
    def test_below_the_threshold_is_unchanged(self):
        self.assertEqual(price(0), 0)
        self.assertEqual(price(9999), 9999)

    def test_the_threshold_itself_is_discounted(self):
        self.assertEqual(price(10000), 9000)
        self.assertEqual(price(10001), 9001)

    def test_halves_round_up(self):
        self.assertEqual(price(10005), 9005)
        self.assertEqual(price(10004), 9004)
        self.assertEqual(price(10006), 9005)

    def test_large_order(self):
        self.assertEqual(price(20000), 18000)

    def test_negative_total_is_rejected(self):
        with self.assertRaises(ValueError):
            price(-1)


if __name__ == "__main__":
    unittest.main()
