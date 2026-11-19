import unittest

from reports import revenue_by_month


class TestReports(unittest.TestCase):
    def test_by_month(self):
        self.assertEqual(revenue_by_month()["2026-01"], 2400)


if __name__ == "__main__":
    unittest.main()
