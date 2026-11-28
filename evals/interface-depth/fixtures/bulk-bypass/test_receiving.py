import unittest

import inventory
import receiving


class ReceivingTest(unittest.TestCase):
    def test_delivery(self):
        inv = inventory.Inventory()
        receiving.book_delivery(inv, {"A": 3})
        self.assertEqual(inv.level("A"), 3)


if __name__ == "__main__":
    unittest.main()
