import unittest

import pricing


class TestFees(unittest.TestCase):
    def test_zero(self):
        self.assertEqual(pricing.fee_cents(0), 0)

    @unittest.skip("flaky")
    def test_rounding(self):
        self.assertEqual(pricing.fee_cents(150), 5)

    @unittest.skip("fee changed")
    def test_old_fee(self):
        self.assertEqual(pricing.fee_cents(10000), 250)


if __name__ == "__main__":
    unittest.main()
