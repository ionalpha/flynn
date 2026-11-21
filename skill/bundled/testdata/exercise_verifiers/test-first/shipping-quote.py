import glob
import os
import shutil
import subprocess
import sys
from fractions import Fraction

sys.path.insert(0, ".")
fails = []
REF = """from fractions import Fraction
T = {"US": 0, "EU": 19, "UK": 20}
S = {"US": 500, "EU": 700, "UK": 900}


def up(n, d):
    return (2 * n + d) // (2 * d)


def quote(lines, region, coupon=None):
    if not lines%(E)s:
        raise ValueError("empty")
    for u, q in lines:
        if type(u) is not int or type(q) is not int or u < %(U)d or q < %(Q)d:
            raise ValueError("line")
    sub = sum(u * q for u, q in lines)
    if coupon is None:
        disc = 0
    elif coupon == "TEN":
        disc = %(TEN)s
    elif coupon == "FIVE":
        disc = %(FIVE)s
    else:
        %(BAD)s
    d = sub - disc
    if %(RG)s:
        raise ValueError(region)
    ship = 0 if %(TH)s %(CMP)s 5000 else S.get(region, 500)
    tax = %(TAX)s
    return {"subtotal": sub, "discount": disc, "shipping": ship, "tax": tax, "total": d + ship + tax}
"""
GOOD = dict(E="", U=0, Q=1, TEN="round(Fraction(sub, 10))", FIVE="min(500, sub)", BAD="raise ValueError(coupon)",
            RG="region not in S", TH="d", CMP=">=", TAX="up((d + ship) * T.get(region, 0), 100)")
MUT = {
    "rounds the TEN discount half up": dict(TEN="up(sub, 10)"),
    "truncates the TEN discount": dict(TEN="sub // 10"),
    "lets FIVE exceed the subtotal": dict(FIVE="500"),
    "makes shipping free only above 5000": dict(CMP=">"),
    "tests free shipping on the undiscounted subtotal": dict(TH="sub"),
    "leaves shipping out of the tax": dict(TAX="up(d * T.get(region, 0), 100)"),
    "taxes the undiscounted subtotal": dict(TAX="up((sub + ship) * T.get(region, 0), 100)"),
    "rounds tax half to even": dict(TAX="round(Fraction((d + ship) * T.get(region, 0), 100))"),
    "accepts a quantity of 0": dict(Q=0),
    "accepts a negative price": dict(U=-10**9),
    "accepts an empty order": dict(E=" and False"),
    "accepts an unknown region": dict(RG="False"),
    "checks the region only when shipping is charged": dict(RG="region not in S and d < 5000"),
    "ignores an unknown coupon": dict(BAD="disc = 0"),
}
ns = {}
exec(REF % GOOD, ns)
LINES = [[(1005, 1)], [(995, 5)], [(850, 1)], [(2850, 1)], [(1999, 3), (250, 2)], [(4999, 1)], [(5000, 1)], [(5500, 1)],
         [(5556, 1)], [(2500, 2)], [(0, 1)], [(300, 2), (0, 5)], [(300, 1)], [], [(100, 0)], [(100, -1)], [(-1, 1)], [(100, 1.5)]]


def call(f, *a):
    try:
        return f(*a)
    except ValueError:
        return "ValueError"
    except Exception as e:
        return "raised " + type(e).__name__


try:
    import quote

    bad = []
    for ln in LINES:
        for rg in ("US", "EU", "UK", "FR"):
            for cp in (None, "TEN", "FIVE", "ten", "HALF"):
                got, want = call(quote.quote, ln, rg, cp), call(ns["quote"], ln, rg, cp)
                if got != want:
                    bad.append("quote(%r, %r, %r) is %r, want %r" % (ln, rg, cp, got, want))
    fails += bad[:3] + (["...and %d more" % (len(bad) - 3)] if len(bad) > 3 else [])
except Exception as e:
    fails.append("quote.quote is not usable: %r" % (e,))


def run(d):
    p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
                       cwd=d, capture_output=True, encoding="utf-8", errors="replace", timeout=30)
    return p.returncode, p.stderr


if not glob.glob("test*.py"):
    fails.append("there is no test file")
elif not fails:
    rc, out = run(".")
    if rc != 0:
        fails.append("the tests fail on the implementation: " + out[-300:])
    else:
        for i, (what, over) in enumerate(MUT.items()):
            d = "_mut_%d" % i
            shutil.copytree(".", d, ignore=shutil.ignore_patterns("_mut_*", "__pycache__", ".git", ".flynn-ab"))
            with open(os.path.join(d, "quote.py"), "w", newline="\n", encoding="utf-8") as f:
                f.write(REF % dict(GOOD, **over))
            if run(d)[0] == 0:
                fails.append("the tests still pass when the code " + what)
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
