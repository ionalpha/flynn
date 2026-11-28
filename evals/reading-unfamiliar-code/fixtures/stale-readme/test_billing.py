import unittest

import billing
import ledger


class Charge(unittest.TestCase):
    def setUp(self):
        del ledger.ENTRIES[:]

    def test_charge_posts_one_entry(self):
        billing.charge({"id": "A1", "account": "acct-7", "total_cents": 2599})
        self.assertEqual(len(ledger.ENTRIES), 1)
        self.assertEqual(ledger.ENTRIES[0][0], "acct-7")


if __name__ == "__main__":
    unittest.main()
