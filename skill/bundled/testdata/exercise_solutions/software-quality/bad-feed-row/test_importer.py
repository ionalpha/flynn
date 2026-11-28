import os
import shutil
import unittest

from importer import load_prices


class LoadPricesTest(unittest.TestCase):
    def setUp(self):
        self.dir = os.path.abspath("test-tmp")
        shutil.rmtree(self.dir, ignore_errors=True)
        os.makedirs(self.dir)
        self.addCleanup(shutil.rmtree, self.dir, True)

    def write(self, text):
        p = os.path.join(self.dir, "feed.csv")
        with open(p, "w", encoding="utf-8") as f:
            f.write(text)
        return p

    def test_reads_every_row(self):
        prices, rejected = load_prices(self.write("A-1,10.50\nC-3,7.25\n"))
        self.assertEqual(prices, {"A-1": 10.5, "C-3": 7.25})
        self.assertEqual(rejected, [])

    def test_a_bad_row_is_rejected_and_reported(self):
        prices, rejected = load_prices(self.write("A-1,10.50\nB-2,N/A\n"))
        self.assertEqual(prices, {"A-1": 10.5})
        self.assertEqual(rejected, [(2, ["B-2", "N/A"])])


if __name__ == "__main__":
    unittest.main()
