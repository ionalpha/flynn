import unittest

from interest import balance_after


class BalanceAfter(unittest.TestCase):
    def test_no_months(self):
        self.assertEqual(balance_after(100000, 12, 0), 100000)

    def test_one_year(self):
        self.assertEqual(balance_after(100000, 12, 12), 112683)

    def test_two_years(self):
        self.assertEqual(balance_after(100000, 12, 24), 126973)


if __name__ == "__main__":
    unittest.main()
