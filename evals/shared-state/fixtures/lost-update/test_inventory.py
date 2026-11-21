import unittest

import inventory
from store import Store


class TestReserve(unittest.TestCase):
    def test_takes_stock(self):
        s = Store({"mug": 3})
        inventory.reserve(s, "mug", 2)
        self.assertEqual(s.get("mug")[0], 1)

    def test_out_of_stock(self):
        s = Store({"mug": 1})
        with self.assertRaises(inventory.OutOfStock):
            inventory.reserve(s, "mug", 2)


if __name__ == "__main__":
    unittest.main()
