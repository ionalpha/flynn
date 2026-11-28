import unittest

import pricing


class TestFees(unittest.TestCase):
    def test_zero(self):
        self.assertEqual(pricing.fee_cents(0), 0)

    def test_rounding(self):
        self.assertEqual(pricing.fee_cents(150), 4)

    def test_old_fee(self):
        self.assertEqual(pricing.fee_cents(10000), 300)


if __name__ == "__main__":
    unittest.main()
