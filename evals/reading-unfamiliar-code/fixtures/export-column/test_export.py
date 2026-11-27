import csv
import os
import tempfile
import unittest

from export import write_orders


class Export(unittest.TestCase):
    def test_one_row_per_order(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "orders.csv")
            write_orders(path)
            with open(path, newline="", encoding="utf-8") as f:
                rows = list(csv.reader(f))
        self.assertEqual([r[0] for r in rows], ["order_id", "A1", "A2", "A3"])


if __name__ == "__main__":
    unittest.main()
