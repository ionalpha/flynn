import unittest
from unittest import mock

import shipping

ORDER = {"postcode": "2011", "promo": "SPRING", "items": [{"cents": 2500, "kg": 1.5}]}


class TestQuote(unittest.TestCase):
    @mock.patch("shipping.promos.percent_off", return_value=10)
    @mock.patch("shipping.rates.base_rate", return_value=1000)
    @mock.patch("shipping.zones.zone_for", return_value="B")
    def test_promo_comes_off_the_rate(self, zone_for, base_rate, percent_off):
        self.assertEqual(shipping.quote(ORDER), 900)
        zone_for.assert_called_once_with("2011")
        base_rate.assert_called_once_with("B", 1.5)

    @mock.patch("shipping.promos.percent_off", return_value=0)
    @mock.patch("shipping.rates.base_rate", return_value=700)
    @mock.patch("shipping.zones.zone_for", return_value="A")
    def test_no_promo(self, zone_for, base_rate, percent_off):
        self.assertEqual(shipping.quote(dict(ORDER, promo=None)), 700)


if __name__ == "__main__":
    unittest.main()
