import unittest

import orders


class OrdersFor(unittest.TestCase):
    def test_one_list_per_requested_customer_in_order(self):
        got = orders.orders_for([5, 2, 5, 4000])
        self.assertEqual([len(g) for g in got], [1, 2, 1, 0])
        self.assertEqual(got[0], got[2])
        self.assertEqual(got[1][0][0], "o2-0")


if __name__ == "__main__":
    unittest.main()
