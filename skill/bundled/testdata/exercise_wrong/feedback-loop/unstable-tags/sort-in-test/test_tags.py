import unittest

from tags import normalise


class NormaliseTest(unittest.TestCase):
    def test_duplicates_collapse(self):
        self.assertEqual(sorted(normalise(["Go", "go ", "rust"])), ["go", "rust"])

    def test_alphabetical(self):
        self.assertEqual(sorted(normalise(["zeta", "Alpha", "mid", "beta"])), ["alpha", "beta", "mid", "zeta"])

    def test_blank_tags_are_dropped(self):
        self.assertEqual(sorted(normalise(["", "  ", "a"])), ["a"])

    def test_nothing_in_nothing_out(self):
        self.assertEqual(sorted(normalise([])), [])


if __name__ == "__main__":
    unittest.main()
