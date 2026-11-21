import unittest

from app import quote


class TestApp(unittest.TestCase):
    def test_quote(self):
        self.assertEqual(quote([("tea", 1)]), 450)


if __name__ == "__main__":
    unittest.main()
