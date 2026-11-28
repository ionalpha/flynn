import unittest

import orders
from nightly import expire
from warehouse import pick_list


class Lifecycle(unittest.TestCase):
    def test_paid_orders_are_picked(self):
        a = orders.Order("A", 1)
        orders.pay(a)
        self.assertEqual(pick_list([a, orders.Order("B", 1)]), ["A"])

    def test_old_unpaid_orders_expire(self):
        a = orders.Order("A", 1)
        expire([a], 20)
        self.assertEqual(a.status, "cancelled")


if __name__ == "__main__":
    unittest.main()
