import unittest

from report import render


class ReportTest(unittest.TestCase):
    def test_render(self):
        rows = [{"name": "a", "qty": 2, "price": 3}, {"name": "b", "qty": 1, "price": 4}]
        self.assertEqual(render(rows), '{"count": 2, "total": 10}')


if __name__ == "__main__":
    unittest.main()
