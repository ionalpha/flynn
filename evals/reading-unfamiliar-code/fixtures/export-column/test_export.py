import csv
import os
import shutil
import unittest

from export import write_orders


class Export(unittest.TestCase):
    def setUp(self):
        self.dir = os.path.abspath("test-tmp")
        shutil.rmtree(self.dir, ignore_errors=True)
        os.makedirs(self.dir)
        self.addCleanup(shutil.rmtree, self.dir, True)

    def test_one_row_per_order(self):
        path = os.path.join(self.dir, "orders.csv")
        write_orders(path)
        with open(path, newline="", encoding="utf-8") as f:
            rows = list(csv.reader(f))
        self.assertEqual([r[0] for r in rows], ["order_id", "A1", "A2", "A3"])


if __name__ == "__main__":
    unittest.main()
