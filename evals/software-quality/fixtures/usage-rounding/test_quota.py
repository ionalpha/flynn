import unittest

import quota


class PercentTest(unittest.TestCase):
    def test_whole_percent(self):
        self.assertEqual(quota.percent(1, 3), 33)
        self.assertEqual(quota.percent(0, 10), 0)
        self.assertEqual(quota.percent(10, 10), 100)

    def test_no_limit_is_zero(self):
        self.assertEqual(quota.percent(5, 0), 0)

    def test_remaining_never_negative(self):
        self.assertEqual(quota.remaining(12, 10), 0)
        self.assertEqual(quota.remaining(4, 10), 6)


if __name__ == "__main__":
    unittest.main()
