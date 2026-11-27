import unittest
from unittest import mock

import checkout
import refunds


class CheckoutTest(unittest.TestCase):
    def test_bulk_line(self):
        self.assertEqual(checkout.total([{"cents": 100, "qty": 10}]), 900)

    def test_small_line(self):
        self.assertEqual(checkout.total([{"cents": 100, "qty": 2}]), 200)

    def test_old_checkout(self):
        with mock.patch("flags.enabled", return_value=False):
            self.assertEqual(checkout.total([{"cents": 100, "qty": 2}]), 194)

    def test_partial_refund(self):
        self.assertEqual(refunds.partial_refund(25, 10), 3)


if __name__ == "__main__":
    unittest.main()
