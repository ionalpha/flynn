import unittest

from duration import parse_duration


class ParseDuration(unittest.TestCase):
    def test_hours_and_minutes(self):
        self.assertEqual(parse_duration("1h30m"), 5400)

    def test_seconds(self):
        self.assertEqual(parse_duration("90s"), 90)

    def test_empty_is_rejected(self):
        with self.assertRaises(ValueError):
            parse_duration("")


if __name__ == "__main__":
    unittest.main()
