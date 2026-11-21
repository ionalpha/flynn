import unittest

import pricing


class HelperTest(unittest.TestCase):
    def test_discount_tiers(self):
        self.assertEqual(pricing._discount(100.0, 50, "standard"), 10.0)

    def test_shipping_free_when_empty(self):
        self.assertEqual(pricing._shipping(0.0, 0, "DE"), 0.0)

    def test_tax_zero_outside_gb(self):
        self.assertEqual(pricing._tax(10.0, "FR"), 0.0)


if __name__ == "__main__":
    unittest.main()
