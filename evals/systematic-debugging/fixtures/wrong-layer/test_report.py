import unittest

from report import format_total


class TestReport(unittest.TestCase):
    def test_settlement_matches_the_provider(self):
        rows = [{"amount": "1.13"}, {"amount": "2.01"}, {"amount": "0.29"}]
        self.assertEqual(format_total(rows), "3.43")


if __name__ == "__main__":
    unittest.main()
