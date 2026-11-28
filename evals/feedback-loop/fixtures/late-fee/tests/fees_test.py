import unittest
from datetime import date

from fees import late_fee


class LateFeeTest(unittest.TestCase):
    def test_a_very_late_book_costs_the_cap(self):
        self.assertEqual(late_fee(date(2026, 1, 1), date(2026, 3, 1)), 500)
