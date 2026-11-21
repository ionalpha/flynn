import os
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))


class TestBoundaries(unittest.TestCase):
    def test_domain_does_not_import_db(self):
        for name in os.listdir(os.path.join(HERE, "domain")):
            if name.endswith(".py"):
                src = open(os.path.join(HERE, "domain", name), encoding="utf-8").read()
                self.assertNotIn("from db", src, name)
                self.assertNotIn("import db", src, name)


if __name__ == "__main__":
    unittest.main()
