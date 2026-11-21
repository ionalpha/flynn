import unittest

from quote import quote


class Frozen(unittest.TestCase):
    def test_ten_percent(self):
        self.assertEqual(quote([(1005, 1)], "US", "TEN")["discount"], 101)

    def test_eu_tax(self):
        self.assertEqual(quote([(1000, 1)], "EU")["tax"], 340)

    def test_threshold(self):
        self.assertEqual(quote([(5000, 1)], "US")["shipping"], 500)

    def test_unknown_region(self):
        self.assertEqual(quote([(1000, 1)], "FR")["shipping"], 500)
