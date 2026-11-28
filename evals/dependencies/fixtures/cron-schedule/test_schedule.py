import unittest
from datetime import datetime

import schedule


class ScheduleTest(unittest.TestCase):
    def test_every_quarter_hour(self):
        got = schedule.next_run("*/15 * * * *", datetime(2026, 3, 1, 10, 7))
        self.assertEqual(got, datetime(2026, 3, 1, 10, 15))

    def test_weekday_morning_skips_the_weekend(self):
        got = schedule.next_run("0 9 * * 1-5", datetime(2026, 3, 6, 17, 0))
        self.assertEqual(got, datetime(2026, 3, 9, 9, 0))


if __name__ == "__main__":
    unittest.main()
