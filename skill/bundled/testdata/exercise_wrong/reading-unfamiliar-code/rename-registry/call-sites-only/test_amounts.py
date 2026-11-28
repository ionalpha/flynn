import unittest

from amounts import parse_cents


class ParseAmount(unittest.TestCase):
    def test_whole_and_fraction(self):
        self.assertEqual(parse_cents("12.50"), 1250)

    def test_no_fraction(self):
        self.assertEqual(parse_cents("3"), 300)

    def test_rejects_text(self):
        with self.assertRaises(ValueError):
            parse_cents("abc")


if __name__ == "__main__":
    unittest.main()
