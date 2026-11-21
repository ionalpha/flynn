import hashlib
import os
import random
import re
import subprocess
import sys

sys.path.insert(0, ".")
TEST_SHA = "58fa7e54b87a1d73d8fc5fbb02cc2034765a2babc0015c8f152d9917f5a8d561"
PEOPLE = ["Ana", "Ben", "Caro", "Dev"]
# The fortnight the project's test uses, and a second one so the fix has to be general.
SCHEDULES = [
    {"Ana": {6, 10, 11}, "Ben": {9, 10, 11, 12, 13}, "Caro": {3, 4, 7, 9, 10}, "Dev": {4, 6, 7, 9}},
    {"Ana": {0, 1, 3, 6, 7}, "Ben": {1, 11, 12}, "Caro": {0, 3, 5, 7, 8, 9, 10}, "Dev": {0, 5, 6, 7, 8}},
]
NOISE = ("__pycache__", ".pytest_cache", ".flynn-ab")
fails = []

if hashlib.sha256(open("test_rota.py", "rb").read().replace(b"\r\n", b"\n")).hexdigest() != TEST_SHA:
    fails.append("test_rota.py was edited; it builds the rota from an unseeded generator on purpose, so the test is right and build_rota is what fails on some draws")
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
    if p.returncode != 0 or not ran or int(ran.group(1)) < 1:
        fails.append("python -m unittest failed on run %d of 12 with no change in between" % (run + 1))
        break
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
