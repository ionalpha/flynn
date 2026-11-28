import unittest

import totals


class TotalsTest(unittest.TestCase):
    def test_revenue(self):
        self.assertEqual(totals.revenue_cents(), 510)


if __name__ == "__main__":
    unittest.main()
