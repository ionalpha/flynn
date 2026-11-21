import unittest

from gateway import Gateway
from payments import pay


class PayTest(unittest.TestCase):
    def test_a_clean_charge_is_paid(self):
        gateway, order = Gateway(), {"id": 7, "cents": 1250}
        pay(gateway, order)
        self.assertEqual(order["state"], "paid")
        self.assertEqual(gateway.charges_for(7), 1)

    def test_a_request_that_never_arrived_is_retried(self):
        gateway, order = Gateway(["lost"]), {"id": 7, "cents": 1250}
        pay(gateway, order)
        self.assertEqual(order["state"], "paid")
        self.assertEqual(gateway.charges_for(7), 1)


if __name__ == "__main__":
    unittest.main()
