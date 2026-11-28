import unittest

from duration import parse_duration


class ParseDuration(unittest.TestCase):
    def test_valid_durations(self):
        for text, want in (
            ("1h30m", 5400),
            ("90s", 90),
            ("2h", 7200),
            ("1h1m1s", 3661),
            ("0s", 0),
            ("100m", 6000),
        ):
            with self.subTest(text=text):
                self.assertEqual(parse_duration(text), want)

    def test_invalid_durations(self):
        for text in (
            "",
            "h",
            "30",
            "1m1h",
            "1h1h",
            "-5s",
            "1.5h",
            " 1h",
            "1h 30m",
            "1H",
            "1h\n",
            "1d",
        ):
            with self.subTest(text=text):
                with self.assertRaises(ValueError):
                    parse_duration(text)


if __name__ == "__main__":
    unittest.main()
