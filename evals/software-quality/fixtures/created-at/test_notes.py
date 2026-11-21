import os
import tempfile
import unittest

import notes


class AddTest(unittest.TestCase):
    def test_add_numbers_notes_in_order(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "n.json")
            self.assertEqual(notes.add(p, "a")["id"], 1)
            self.assertEqual(notes.add(p, "b")["id"], 2)


if __name__ == "__main__":
    unittest.main()
