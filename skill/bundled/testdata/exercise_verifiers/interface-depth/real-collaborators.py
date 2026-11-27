import os
import re
import shutil
import subprocess
import sys

sys.path.insert(0, ".")
fails = []


def suite(cwd):
    return subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test_*.py"], cwd=cwd, capture_output=True, text=True)


try:
    import shipping

    def order(postcode, promo, *items):
        return {"postcode": postcode, "promo": promo, "items": [{"cents": c, "kg": k} for c, k in items]}

    cases = [
        (order("2011", "SPRING", (2500, 1.5)), 855, "an ordinary order changed"),
        (order("1011", None, (2500, 3.0), (100, 0.5)), 880, "an ordinary order changed"),
        (order("9911", "friend", (1, 1)), 832, "an ordinary order changed"),
        (order("1011", None, (6000, 2), (3999, 1)), 760, "goods of 9999 cents must still pay for shipping"),
        (order("1011", None, (6000, 2), (4000, 1)), 0, "goods of exactly 10000 cents ship free"),
        (order("3011", "SPRING", (25000, 4)), 0, "goods over 10000 cents ship free, promo or not"),
    ]
    for o, want, why in cases:
        got = shipping.quote(o)
        if got != want:
            fails.append("quote(%s) is %r, want %r: %s" % (o["items"], got, want, why))
    p = suite(".")
    if p.returncode != 0:
        fails.append("the suite fails: " + p.stderr[-300:])
    work = ".mutation-%d" % os.getpid()
    os.makedirs(work)
    try:
        shutil.copytree(".", os.path.join(work, "t"), ignore=shutil.ignore_patterns(".*", "__pycache__"))
        path = os.path.join(work, "t", "rates.py")
        src = open(path, encoding="utf-8").read()
        mutated = re.sub(r'"base": (\d+)', lambda m: '"base": %d' % (int(m.group(1)) + 1), src)
        if mutated == src:
            fails.append("rates.py no longer has the rate cards this check mutates")
        open(path, "w", encoding="utf-8").write(mutated)
        if suite(os.path.join(work, "t")).returncode == 0:
            fails.append("changing every base rate by one cent fails no test: no test prices a parcel through the real rate cards")
    finally:
        shutil.rmtree(work, ignore_errors=True)
except Exception as e:
    fails.append("shipping could not be exercised: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
