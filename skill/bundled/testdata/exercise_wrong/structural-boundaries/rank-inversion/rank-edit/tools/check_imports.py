"""Fails when a module imports one of equal or higher rank."""
import ast
import os
import sys

RANK = {"money": 3, "customers": 1, "pricing": 1, "invoices": 2}


def imports(path):
    tree = ast.parse(open(path, encoding="utf-8").read())
    for node in tree.body:
        if isinstance(node, ast.Import):
            for alias in node.names:
                yield alias.name.split(".")[0], node.lineno
        elif isinstance(node, ast.ImportFrom) and node.module:
            yield node.module.split(".")[0], node.lineno


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    bad = []
    for name, rank in RANK.items():
        for target, line in imports(os.path.join(root, name + ".py")):
            if target in RANK and RANK[target] >= rank:
                bad.append("%s.py:%d imports %s, which is not below it" % (name, line, target))
    for b in bad:
        print(b, file=sys.stderr)
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
