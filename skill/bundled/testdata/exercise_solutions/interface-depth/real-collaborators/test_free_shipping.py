import unittest

import shipping


class TestFreeShipping(unittest.TestCase):
    def test_below_threshold_pays_the_rate_card(self):
        order = {"postcode": "1011", "promo": None, "items": [{"cents": 9999, "kg": 3}]}
        self.assertEqual(shipping.quote(order), 760)

    def test_threshold_ships_free(self):
        order = {"postcode": "1011", "promo": None, "items": [{"cents": 10000, "kg": 3}]}
        self.assertEqual(shipping.quote(order), 0)


if __name__ == "__main__":
    unittest.main()
