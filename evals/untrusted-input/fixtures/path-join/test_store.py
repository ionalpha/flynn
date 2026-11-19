import unittest

from store import read_attachment


class StoreTest(unittest.TestCase):
    def test_reads_a_file(self):
        self.assertEqual(read_attachment("a.txt").strip(), b"hello a")

    def test_reads_a_nested_file(self):
        self.assertEqual(read_attachment("sub/b.txt").strip(), b"hello b")


if __name__ == "__main__":
    unittest.main()
