import unittest

from quote import quote


class Some(unittest.TestCase):
    def test_ten_tie(self):
        self.assertEqual(quote([(1005, 1)], "US", "TEN")["discount"], 100)

    def test_eu_tax(self):
        self.assertEqual(quote([(1000, 1)], "EU")["tax"], 323)

    def test_unknown_region(self):
        with self.assertRaises(ValueError):
            quote([(1000, 1)], "FR")
