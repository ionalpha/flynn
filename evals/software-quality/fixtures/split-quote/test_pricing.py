import unittest

from pricing import quote


class QuoteTest(unittest.TestCase):
    def test_small_gb_order_pays_shipping_and_tax(self):
        q = quote([(10.0, 2)])
        self.assertEqual(q["shipping"], 4.99)
        self.assertEqual(q["tax"], 5.0)
        self.assertEqual(q["total"], 29.99)

    def test_free_shipping_over_the_threshold(self):
        self.assertEqual(quote([(60.0, 2)])["shipping"], 0.0)

    def test_volume_discount(self):
        q = quote([(2.0, 50)])
        self.assertEqual(q["discount"], 10.0)

    def test_export_pays_no_tax(self):
        q = quote([(10.0, 2)], country="DE")
        self.assertEqual(q["tax"], 0.0)
        self.assertEqual(q["total"], 34.99)

    def test_empty_order(self):
        self.assertEqual(quote([])["total"], 0.0)


if __name__ == "__main__":
    unittest.main()
