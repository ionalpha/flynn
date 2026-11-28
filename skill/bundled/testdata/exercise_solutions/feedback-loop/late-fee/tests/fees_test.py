import unittest
from datetime import date

from fees import late_fee


class LateFeeTest(unittest.TestCase):
    def test_a_very_late_book_costs_the_cap(self):
        self.assertEqual(late_fee(date(2026, 1, 1), date(2026, 3, 1)), 500)

    def test_a_book_returned_on_the_due_date_is_free(self):
        self.assertEqual(late_fee(date(2026, 1, 10), date(2026, 1, 10)), 0)
