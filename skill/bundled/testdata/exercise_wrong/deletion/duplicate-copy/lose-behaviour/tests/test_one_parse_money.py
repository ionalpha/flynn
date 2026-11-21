import ast
import os
import unittest

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


class OneParseMoney(unittest.TestCase):
    def test_single_definition(self):
        found = []
        for d, dirs, files in os.walk(ROOT):
            dirs[:] = [x for x in dirs if x not in ("tests", "__pycache__", ".git")]
            for f in files:
                if f.endswith(".py"):
                    with open(os.path.join(d, f), encoding="utf-8") as fh:
                        tree = ast.parse(fh.read())
                    found += [
                        os.path.join(d, f)
                        for n in ast.walk(tree)
                        if isinstance(n, ast.FunctionDef) and n.name == "parse_money"
                    ]
        self.assertEqual(len(found), 1, found)
