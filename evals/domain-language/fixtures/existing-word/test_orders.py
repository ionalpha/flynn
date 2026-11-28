import unittest

from orders import order_count


class TestOrders(unittest.TestCase):
    def test_count(self):
        self.assertEqual(order_count("c1"), 2)


if __name__ == "__main__":
    unittest.main()
