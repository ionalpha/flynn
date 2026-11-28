import unittest

import orders
import states
import summary


class Orders(unittest.TestCase):
    def test_cancelled_constant(self):
        self.assertEqual(orders.CANCELED, "cancelled")

    def test_a_paid_order_may_be_cancelled(self):
        self.assertTrue(states.can("paid", "cancelled"))

    def test_a_shipped_order_may_not_be_cancelled(self):
        self.assertFalse(states.can("shipped", "cancelled"))

    def test_summary_lines(self):
        rows = [{"id": "1", "status": "new"}, {"id": "2", "status": "cancelled"}, {"id": "3", "status": "cancelled"}]
        self.assertEqual(summary.lines(rows), ["new 1", "paid 0", "shipped 0", "delivered 0", "canceled 2"])


if __name__ == "__main__":
    unittest.main()
