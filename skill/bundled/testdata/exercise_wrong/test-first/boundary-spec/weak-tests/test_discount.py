import unittest

from discount import price_after_discount as price


class PriceAfterDiscount(unittest.TestCase):
    def test_small_order(self):
        self.assertEqual(price(5000), 5000)

    def test_large_order(self):
        self.assertEqual(price(20000), 18000)

    def test_returns_an_int(self):
        self.assertIsInstance(price(12345), int)


if __name__ == "__main__":
    unittest.main()
