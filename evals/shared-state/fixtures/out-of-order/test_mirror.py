import unittest

from mirror import Mirror, on_order_updated
from vendor.orders_api import OrdersApi


class Updates(unittest.TestCase):
    def test_an_event_updates_the_mirror(self):
        api = OrdersApi({"o1": {"status": "paid", "version": 1}})
        m = Mirror()
        on_order_updated(m, api, {"order": "o1", "status": "paid", "version": 1})
        self.assertEqual(m.status("o1"), "paid")

    def test_a_later_event_replaces_an_earlier_one(self):
        api = OrdersApi({"o1": {"status": "shipped", "version": 2}})
        m = Mirror()
        on_order_updated(m, api, {"order": "o1", "status": "paid", "version": 1})
        on_order_updated(m, api, {"order": "o1", "status": "shipped", "version": 2})
        self.assertEqual(m.status("o1"), "shipped")


if __name__ == "__main__":
    unittest.main()
