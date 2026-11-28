import unittest

import web
from domain import orders


class OrdersTest(unittest.TestCase):
    def test_post(self):
        web.post_order({"id": "t1", "email": "a@example.com", "cents": "5"})
        self.assertIn("t1", orders.ORDERS)


if __name__ == "__main__":
    unittest.main()
