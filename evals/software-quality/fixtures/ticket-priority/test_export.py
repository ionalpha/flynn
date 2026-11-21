import csv
import os
import tempfile
import unittest

import export


class ExportTest(unittest.TestCase):
    def test_header_and_first_row(self):
        with tempfile.TemporaryDirectory() as d:
            out = os.path.join(d, "tickets.csv")
            export.write("data/tickets.json", out)
            with open(out, newline="", encoding="utf-8") as f:
                got = list(csv.reader(f))
        self.assertEqual(got[0], ["id", "title", "status", "opened"])
        self.assertEqual(got[1], ["1", "Printer jam on floor 2", "open", "2026-02-03"])
        self.assertEqual(len(got), 4)


if __name__ == "__main__":
    unittest.main()
