import unittest

import runner


class TestReports(unittest.TestCase):
    def test_finance(self):
        lines = [l.rstrip() for l in runner.run("finance").splitlines()]
        self.assertEqual(lines[0].split(), ["Date", "Account", "Amount", "Currency"])


if __name__ == "__main__":
    unittest.main()
