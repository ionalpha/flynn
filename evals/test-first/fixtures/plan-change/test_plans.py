import unittest

import plans


class Plans(unittest.TestCase):
    def test_limits(self):
        self.assertEqual(plans.limit_for("free"), 5)
        self.assertEqual(plans.limit_for("pro"), 20)


if __name__ == "__main__":
    unittest.main()
