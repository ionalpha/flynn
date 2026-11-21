import ast
import os
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))


def db_imports(path):
    found = []
    for node in ast.walk(ast.parse(open(path, encoding="utf-8").read())):
        if isinstance(node, ast.Import):
            names = [a.name for a in node.names]
        elif isinstance(node, ast.ImportFrom) and node.module and node.level == 0:
            names = [node.module]
        else:
            continue
        found += [n for n in names if n == "db" or n.startswith("db.")]
    return found


class TestBoundaries(unittest.TestCase):
    def test_domain_does_not_import_db(self):
        crossings = []
        for root, _, files in os.walk(os.path.join(HERE, "domain")):
            for name in files:
                if name.endswith(".py"):
                    path = os.path.join(root, name)
                    crossings += [(os.path.relpath(path, HERE), n) for n in db_imports(path)]
        self.assertEqual(crossings, [], "ARCHITECTURE.md: domain/ must not import db/")


if __name__ == "__main__":
    unittest.main()
