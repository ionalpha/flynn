import unittest

from amounts import parse_amount


class ParseAmount(unittest.TestCase):
    def test_whole_and_fraction(self):
        self.assertEqual(parse_amount("12.50"), 1250)

    def test_no_fraction(self):
        self.assertEqual(parse_amount("3"), 300)

    def test_rejects_text(self):
        with self.assertRaises(ValueError):
            parse_amount("abc")


if __name__ == "__main__":
    unittest.main()
