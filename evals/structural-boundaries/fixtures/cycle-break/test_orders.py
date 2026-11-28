import unittest

import orders
import stock


class TestOrders(unittest.TestCase):
    def setUp(self):
        stock.ON_HAND.update({"SKU-LAMP": 10, "SKU-DESK": 4, "SKU-MUG": 8})

    def test_confirmation(self):
        self.assertEqual(
            orders.place_order("o1", [("SKU-LAMP", 2)]),
            ["Order o1 confirmed", "SKU-LAMP x2 (restocks in batches of 120)"],
        )

    def test_out_of_stock(self):
        with self.assertRaises(stock.OutOfStock):
            orders.place_order("o2", [("SKU-DESK", 9)])


if __name__ == "__main__":
    unittest.main()
