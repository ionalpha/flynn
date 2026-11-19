import unittest

from slugs import slugify


class SlugTest(unittest.TestCase):
    def test_words(self):
        self.assertEqual(slugify("Hello, World"), "hello-world")

    def test_trims(self):
        self.assertEqual(slugify("  a  b "), "a-b")


if __name__ == "__main__":
    unittest.main()
