import os
import shutil
import unittest

import notes


class AddTest(unittest.TestCase):
    def setUp(self):
        self.dir = os.path.abspath("test-tmp")
        shutil.rmtree(self.dir, ignore_errors=True)
        os.makedirs(self.dir)
        self.addCleanup(shutil.rmtree, self.dir, True)

    def test_add_numbers_notes_in_order(self):
        p = os.path.join(self.dir, "n.json")
        self.assertEqual(notes.add(p, "a")["id"], 1)
        self.assertEqual(notes.add(p, "b")["id"], 2)


if __name__ == "__main__":
    unittest.main()
