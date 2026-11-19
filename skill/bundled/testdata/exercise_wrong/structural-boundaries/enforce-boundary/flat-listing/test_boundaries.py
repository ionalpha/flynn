import ast
import os
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))


class TestBoundaries(unittest.TestCase):
    def test_domain_does_not_import_db(self):
        for name in os.listdir(os.path.join(HERE, "domain")):
            if not name.endswith(".py"):
                continue
            tree = ast.parse(open(os.path.join(HERE, "domain", name), encoding="utf-8").read())
            for node in ast.walk(tree):
                if isinstance(node, ast.ImportFrom):
                    self.assertFalse((node.module or "").split(".")[0] == "db", name)
                if isinstance(node, ast.Import):
                    for a in node.names:
                        self.assertFalse(a.name.split(".")[0] == "db", name)


if __name__ == "__main__":
    unittest.main()
