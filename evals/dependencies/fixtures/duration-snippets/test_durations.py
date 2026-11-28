import unittest

import durations


class DurationTest(unittest.TestCase):
    def test_hours_and_minutes(self):
        self.assertEqual(durations.parse_duration("1h30m"), 5400)

    def test_bad_unit_is_refused(self):
        with self.assertRaises(ValueError):
            durations.parse_duration("5x")

    def test_format(self):
        self.assertEqual(durations.format_duration(5400), "1h30m")


if __name__ == "__main__":
    unittest.main()
