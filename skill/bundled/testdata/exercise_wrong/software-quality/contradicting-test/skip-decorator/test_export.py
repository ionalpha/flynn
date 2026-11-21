import unittest
from decimal import Decimal

from ledger import round_money


class ExportTest(unittest.TestCase):
    @unittest.skip("rounding policy under review")
    def test_round_half(self):
        self.assertEqual(round_money(Decimal("0.125")), Decimal("0.12"))


if __name__ == "__main__":
    unittest.main()
