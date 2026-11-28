import unittest

import money


class TestLocale(unittest.TestCase):
    def test_de(self):
        self.assertEqual(money.fmt(123450, locale="de"), "1.234,50 EUR")

    def test_default(self):
        self.assertEqual(money.fmt(123450), "1,234.50 EUR")


if __name__ == "__main__":
    unittest.main()
