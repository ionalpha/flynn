import os
import unittest

import config


class ConfigTest(unittest.TestCase):
    def test_production_example(self):
        path = os.path.join(os.path.dirname(__file__), "examples", "production.ini")
        self.assertEqual(config.load(path)["port"], "587")


if __name__ == "__main__":
    unittest.main()
