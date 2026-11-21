import unittest

import pricing
import store


class Pricing(unittest.TestCase):
    def setUp(self):
        store.PRICES.clear()

    def test_basket_total(self):
        pricing.set_price("a", 100)
        pricing.set_price("b", 250)
        self.assertEqual(pricing.basket_total(["a", "b", "a"]), 450)

    def test_unknown_sku(self):
        with self.assertRaises(KeyError):
            pricing.get_price("nope")


if __name__ == "__main__":
    unittest.main()
