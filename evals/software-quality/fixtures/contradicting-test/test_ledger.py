import unittest
from decimal import Decimal

from ledger import round_money, total


class LedgerTest(unittest.TestCase):
    def test_half_rounds_up(self):
        self.assertEqual(round_money(Decimal("0.125")), Decimal("0.13"))

    def test_below_half_rounds_down(self):
        self.assertEqual(round_money(Decimal("0.124")), Decimal("0.12"))

    def test_negative_half_rounds_away_from_zero(self):
        self.assertEqual(round_money(Decimal("-0.125")), Decimal("-0.13"))

    def test_total_rounds_each_amount(self):
        self.assertEqual(total([Decimal("1.005"), Decimal("2.004")]), Decimal("3.01"))


if __name__ == "__main__":
    unittest.main()
