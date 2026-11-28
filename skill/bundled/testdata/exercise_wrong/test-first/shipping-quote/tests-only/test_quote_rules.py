import unittest

from quote import quote


def parts(lines, region, coupon=None):
    q = quote(lines, region, coupon)
    return q["discount"], q["shipping"], q["tax"], q["total"]


class Coupons(unittest.TestCase):
    def test_ten_tie_goes_to_the_even_cent_down(self):
        self.assertEqual(parts([(1005, 1)], "US", "TEN"), (100, 500, 0, 905 + 500))

    def test_ten_tie_goes_to_the_even_cent_up(self):
        self.assertEqual(parts([(1015, 1)], "US", "TEN"), (102, 500, 0, 913 + 500))

    def test_five_never_exceeds_the_subtotal(self):
        self.assertEqual(parts([(300, 1)], "US", "FIVE"), (300, 500, 0, 500))

    def test_unknown_coupon_is_refused(self):
        with self.assertRaises(ValueError):
            quote([(1000, 1)], "US", "HALF")


class Shipping(unittest.TestCase):
    def test_free_at_exactly_5000(self):
        self.assertEqual(parts([(5000, 1)], "US"), (0, 0, 0, 5000))

    def test_charged_just_below_5000(self):
        self.assertEqual(parts([(4999, 1)], "US"), (0, 500, 0, 5499))

    def test_threshold_applies_after_the_discount(self):
        self.assertEqual(parts([(5200, 1)], "US", "TEN"), (520, 500, 0, 5180))


class Tax(unittest.TestCase):
    def test_eu_taxes_goods_and_shipping(self):
        self.assertEqual(parts([(1000, 1)], "EU"), (0, 700, 323, 2023))

    def test_tax_follows_the_discount(self):
        self.assertEqual(parts([(1000, 1)], "EU", "FIVE"), (500, 700, 228, 1428))

    def test_tax_tie_goes_up(self):
        self.assertEqual(parts([(2250, 1)], "EU"), (0, 700, 561, 3511))

    def test_uk_rate(self):
        self.assertEqual(parts([(1000, 1)], "UK"), (0, 900, 380, 2280))


class Refusals(unittest.TestCase):
    def test_empty_order(self):
        with self.assertRaises(ValueError):
            quote([], "US")

    def test_zero_quantity(self):
        with self.assertRaises(ValueError):
            quote([(100, 0)], "US")

    def test_negative_price(self):
        with self.assertRaises(ValueError):
            quote([(-1, 1)], "US")

    def test_unknown_region_when_shipping_is_charged(self):
        with self.assertRaises(ValueError):
            quote([(1000, 1)], "FR")

    def test_unknown_region_when_shipping_would_be_free(self):
        with self.assertRaises(ValueError):
            quote([(6000, 1)], "FR")


if __name__ == "__main__":
    unittest.main()
