import unittest

from tax import tax_for


class TaxTest(unittest.TestCase):
    def test_fr(self):
        self.assertEqual(tax_for("FR"), 0.2)

    def test_it(self):
        self.assertEqual(tax_for("IT"), 0.22)


if __name__ == "__main__":
    unittest.main()
