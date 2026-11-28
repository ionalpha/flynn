import unittest
from unittest import mock

import shipping


class TestFreeShipping(unittest.TestCase):
    @mock.patch("shipping.promos.percent_off", return_value=0)
    @mock.patch("shipping.rates.base_rate", return_value=760)
    @mock.patch("shipping.zones.zone_for", return_value="A")
    def test_below_threshold_pays(self, zone_for, base_rate, percent_off):
        order = {"postcode": "1011", "promo": None, "items": [{"cents": 9999, "kg": 3}]}
        self.assertEqual(shipping.quote(order), 760)

    @mock.patch("shipping.promos.percent_off", return_value=0)
    @mock.patch("shipping.rates.base_rate", return_value=760)
    @mock.patch("shipping.zones.zone_for", return_value="A")
    def test_threshold_ships_free(self, zone_for, base_rate, percent_off):
        order = {"postcode": "1011", "promo": None, "items": [{"cents": 10000, "kg": 3}]}
        self.assertEqual(shipping.quote(order), 0)


if __name__ == "__main__":
    unittest.main()
