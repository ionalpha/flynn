import unittest

from tags import normalise


class NormaliseTest(unittest.TestCase):
    def test_duplicates_collapse(self):
        self.assertCountEqual(normalise(["Go", "go ", "rust"]), ["go", "rust"])

    def test_alphabetical(self):
        self.assertCountEqual(normalise(["zeta", "Alpha", "mid", "beta"]), ["alpha", "beta", "mid", "zeta"])

    def test_blank_tags_are_dropped(self):
        self.assertCountEqual(normalise(["", "  ", "a"]), ["a"])

    def test_nothing_in_nothing_out(self):
        self.assertCountEqual(normalise([]), [])


if __name__ == "__main__":
    unittest.main()
