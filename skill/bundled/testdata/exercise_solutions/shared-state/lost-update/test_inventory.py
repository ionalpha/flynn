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

    def test_two_workers_cannot_both_take_the_last_unit(self):
        s = Store({"mug": 1})
        real_get = s.get
        landed = []

        def get(key):
            seen = real_get(key)
            if not landed:
                landed.append(key)
                inventory.reserve(s, key, 1)  # the other worker writes between our read and our write
            return seen

        s.get = get
        with self.assertRaises(inventory.OutOfStock):
            inventory.reserve(s, "mug", 1)
        self.assertEqual(real_get("mug")[0], 0)


if __name__ == "__main__":
    unittest.main()
