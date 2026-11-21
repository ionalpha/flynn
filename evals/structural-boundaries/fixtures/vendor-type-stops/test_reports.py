import unittest

from reports import monthly_lines


class TestReports(unittest.TestCase):
    def test_amount(self):
        self.assertIn("42.00 USD", monthly_lines()[0])


if __name__ == "__main__":
    unittest.main()
