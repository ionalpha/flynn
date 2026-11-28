import unittest

from refunds import Order, refund, refund_item, refunded


class Refunds(unittest.TestCase):
    def test_partial_refund(self):
        o = Order(1000, {"mug": 400})
        refund(o, 300)
        self.assertEqual(refunded(o), 300)

    def test_returned_item(self):
        o = Order(1000, {"mug": 400})
        refund_item(o, "mug")
        self.assertEqual(refunded(o), 400)


if __name__ == "__main__":
    unittest.main()
