import unittest

from money import format_price


class TestMoney(unittest.TestCase):
    def test_usd(self):
        self.assertEqual(format_price(1250, "USD"), "$12.50")


if __name__ == "__main__":
    unittest.main()
