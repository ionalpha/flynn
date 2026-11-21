import unittest

import runner


class TestReports(unittest.TestCase):
    def test_sales(self):
        self.assertEqual(
            runner.run("sales").splitlines()[0].split(),
            ["Region", "Orders", "Cents"],
        )

    def test_finance(self):
        lines = [l.rstrip() for l in runner.run("finance").splitlines()]
        self.assertEqual(lines[0], "Date       | Account    | Amount")
        self.assertEqual(lines[2], "2026-01-03 | Rent       | 1200.00")


if __name__ == "__main__":
    unittest.main()
