import unittest

from shop.customers import Customer, position_in_list


class TestCustomers(unittest.TestCase):
    def test_position(self):
        cs = [Customer("a", "A", "1"), Customer("b", "B", "2")]
        self.assertEqual(position_in_list(cs, "b"), 1)


if __name__ == "__main__":
    unittest.main()
