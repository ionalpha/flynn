import unittest

import rules


class ActionsTest(unittest.TestCase):
    def test_quiet_below_the_first_threshold(self):
        self.assertEqual(rules.actions("starter", 100), [])

    def test_warns_well_past_eighty(self):
        self.assertEqual(rules.actions("starter", 900), ["warn"])

    def test_bills_well_past_the_limit(self):
        self.assertEqual(rules.actions("pro", 9000), ["warn", "bill_overage"])


if __name__ == "__main__":
    unittest.main()
