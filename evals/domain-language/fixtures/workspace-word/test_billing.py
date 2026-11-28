import unittest

import billing


class TestBilling(unittest.TestCase):
    def test_workspace_plan_has_a_seat_limit(self):
        self.assertEqual(billing.account_plan("w1"), {"name": "Acme Design", "seat_limit": 5})

    def test_charges_add_up(self):
        billing.LEDGER.clear()
        billing.charge_account("w1", 500)
        self.assertEqual(billing.charge_account("w1", 250), 750)


if __name__ == "__main__":
    unittest.main()
