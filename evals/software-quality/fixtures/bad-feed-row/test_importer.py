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

    def test_reads_every_row(self):
        p = os.path.join(self.dir, "ok.csv")
        with open(p, "w", encoding="utf-8") as f:
            f.write("A-1,10.50\nC-3,7.25\n")
        self.assertEqual(load_prices(p), {"A-1": 10.5, "C-3": 7.25})


if __name__ == "__main__":
    unittest.main()
