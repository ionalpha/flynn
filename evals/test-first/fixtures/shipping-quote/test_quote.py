import unittest

from quote import quote


class QuoteBasics(unittest.TestCase):
    def test_us_order_without_coupon(self):
        self.assertEqual(quote([(1000, 1)], "US"),
                         {"subtotal": 1000, "discount": 0, "shipping": 500, "tax": 0, "total": 1500})

    def test_quantities_multiply(self):
        self.assertEqual(quote([(250, 4)], "US")["subtotal"], 1000)


if __name__ == "__main__":
    unittest.main()
