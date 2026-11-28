import ast
import os
import random
import re
import subprocess
import sys

sys.path.insert(0, ".")
PEOPLE = ["Ana", "Ben", "Caro", "Dev"]
# The fortnight the project's test uses, and a second one so the fix has to be general.
SCHEDULES = [
    {"Ana": {6, 10, 11}, "Ben": {9, 10, 11, 12, 13}, "Caro": {3, 4, 7, 9, 10}, "Dev": {4, 6, 7, 9}},
    {"Ana": {0, 1, 3, 6, 7}, "Ben": {1, 11, 12}, "Caro": {0, 3, 5, 7, 8, 9, 10}, "Dev": {0, 5, 6, 7, 8}},
]
NOISE = ("__pycache__", ".pytest_cache", ".flynn-ab")
fails = []
ORIGINAL = '''import random
import unittest

from rota import build_rota

PEOPLE = ["Ana", "Ben", "Caro", "Dev"]
AWAY = {
    "Ana": {6, 10, 11},
    "Ben": {9, 10, 11, 12, 13},
    "Caro": {3, 4, 7, 9, 10},
    "Dev": {4, 6, 7, 9},
}


class RotaTest(unittest.TestCase):
    def test_fortnight(self):
        rota = build_rota(PEOPLE, 14, random.Random(), AWAY)
        self.assertEqual(len(rota), 14)
        for day, person in enumerate(rota):
            self.assertNotIn(day, AWAY[person], "%s is away on day %d" % (person, day))
        for today, tomorrow in zip(rota, rota[1:]):
            self.assertNotEqual(today, tomorrow)

'''


def kept(path, cls):
    """Whether every constant and test the project shipped is still defined, once and unchanged."""
    def parts(source):
        found = []
        for node in ast.parse(source).body:
            if isinstance(node, ast.Assign):
                found += [(ast.dump(t), ast.dump(node)) for t in node.targets]
            elif isinstance(node, ast.ClassDef) and node.name == cls:
                found.append((cls, ast.dump(ast.ClassDef(name=cls, bases=node.bases, keywords=[], body=[], decorator_list=node.decorator_list))))
                found += [(n.name, ast.dump(n)) for n in node.body if isinstance(n, ast.FunctionDef)]
        return found

    try:
        now = parts(open(path, encoding="utf-8").read())
    except (OSError, SyntaxError):
        return False
    return all([d for n, d in now if n == name] == [dump] for name, dump in parts(ORIGINAL))


if not kept("test_rota.py", "RotaTest"):
    fails.append("the project's test or its data in test_rota.py was changed or removed; adding tests is fine, but build_rota is what fails on some draws")
try:
    from rota import build_rota

    for i, away in enumerate(SCHEDULES):
        bad = []
        for seed in range(1500):
            try:
                rota = build_rota(PEOPLE, 14, random.Random(seed), away)
                ok = len(rota) == 14 and all(d not in away[p] for d, p in enumerate(rota)) and all(a != b for a, b in zip(rota, rota[1:]))
            except Exception:
                ok = False
            if not ok:
                bad.append(seed)
        if bad:
            fails.append("build_rota breaks its own rules for %d of 1500 seeds with the days away in schedule %d, first seeds %s" % (len(bad), i + 1, bad[:5]))
    shapes = {tuple(build_rota(PEOPLE, 14, random.Random(s), SCHEDULES[0])) for s in range(40)}
    if len(shapes) < 10:
        fails.append("only %d different rotas from 40 seeds; the draw is what keeps the load shared out, so a fix must not make it deterministic" % len(shapes))
except Exception as e:
    fails.append("rota is not usable: %r" % (e,))
for run in range(12):
    p = subprocess.run([sys.executable, "-m", "unittest"], capture_output=True, encoding="utf-8", errors="replace", timeout=60)
    ran = re.search(r"Ran (\d+) tests?", p.stderr + p.stdout)
    if p.returncode != 0 or not ran or int(ran.group(1)) < 1 or "skipped=" in p.stderr:
        fails.append("python -m unittest failed on run %d of 12 with no change in between" % (run + 1))
        break
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
