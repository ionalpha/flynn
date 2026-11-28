import unittest

from mirror import Mirror, on_order_updated
from vendor.orders_api import ApiDown, OrdersApi


def ev(order, status, version):
    return {"order": order, "status": status, "version": version}


class Updates(unittest.TestCase):
    def test_an_event_updates_the_mirror(self):
        api = OrdersApi({"o1": {"status": "paid", "version": 1}})
        m = Mirror()
        on_order_updated(m, api, ev("o1", "paid", 1))
        self.assertEqual(m.status("o1"), "paid")

    def test_a_later_event_replaces_an_earlier_one(self):
        api = OrdersApi({"o1": {"status": "shipped", "version": 2}})
        m = Mirror()
        on_order_updated(m, api, ev("o1", "paid", 1))
        on_order_updated(m, api, ev("o1", "shipped", 2))
        self.assertEqual(m.status("o1"), "shipped")

    def test_an_old_event_after_a_new_one_changes_nothing(self):
        api = OrdersApi({"o1": {"status": "shipped", "version": 2}})
        m = Mirror()
        on_order_updated(m, api, ev("o1", "shipped", 2))
        on_order_updated(m, api, ev("o1", "paid", 1))
        self.assertEqual(m.status("o1"), "shipped")

    def test_a_dropped_event_is_made_up_by_reading_the_service(self):
        api = OrdersApi({"o1": {"status": "delivered", "version": 3}})
        m = Mirror()
        on_order_updated(m, api, ev("o1", "paid", 1))
        on_order_updated(m, api, ev("o1", "shipped", 2))
        self.assertEqual(m.status("o1"), "delivered")

    def test_a_replica_that_is_behind_does_not_take_the_mirror_back(self):
        api = OrdersApi({"o1": {"status": "delivered", "version": 3}})
        m = Mirror()
        on_order_updated(m, api, ev("o1", "delivered", 3))
        api.orders["o1"] = {"status": "shipped", "version": 2}
        on_order_updated(m, api, ev("o1", "shipped", 2))
        self.assertEqual(m.status("o1"), "delivered")

    def test_an_outage_keeps_the_row(self):
        api = OrdersApi({"o1": {"status": "delivered", "version": 3}})
        m = Mirror()
        on_order_updated(m, api, ev("o1", "delivered", 3))
        api.down = True
        with self.assertRaises(ApiDown):
            on_order_updated(m, api, ev("o1", "paid", 1))
        self.assertEqual(m.status("o1"), "delivered")


if __name__ == "__main__":
    unittest.main()
