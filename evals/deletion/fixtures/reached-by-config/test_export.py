import unittest

import export


class TestExport(unittest.TestCase):
    def test_csv(self):
        self.assertEqual(export.run("csv", [["a", "b,c"]]), 'a,"b,c"')


if __name__ == "__main__":
    unittest.main()
