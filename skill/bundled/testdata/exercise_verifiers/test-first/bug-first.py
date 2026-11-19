import os
import re
import shutil
import subprocess
import sys
from fractions import Fraction

sys.path.insert(0, ".")
fails = []


def ref(p, rate, m):
    v = p * (1 + Fraction(rate, 1200)) ** m
    return (2 * v.numerator + v.denominator) // (2 * v.denominator)


try:
    import interest

    for p, rate in ((100000, 12), (250050, 7), (99, 30)):
        for m in (0, 1, 5, 11, 12, 13, 18, 23, 24, 30):
            got = interest.balance_after(p, rate, m)
            if got != ref(p, rate, m):
                fails.append("balance_after(%d, %d, %d) is %r, want %d" % (p, rate, m, got, ref(p, rate, m)))
except Exception as e:
    fails.append("interest.balance_after is not usable: %r" % (e,))

BUGGY = """from fractions import Fraction


def balance_after(principal_cents, annual_rate_percent, months):
    balance = Fraction(principal_cents)
    for _ in range(months // 12 * 12):
        balance *= 1 + Fraction(annual_rate_percent, 1200)
    return (2 * balance.numerator + balance.denominator) // (2 * balance.denominator)
"""


def run(d):
    p = subprocess.run(
        [sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
        cwd=d, capture_output=True, text=True, timeout=30,
    )
    return p.returncode, p.stderr


rc, out = run(".")
if rc != 0:
    fails.append("the tests fail on the fixed code: " + out[-300:])
else:
    d = "_mut_original"
    shutil.copytree(".", d, ignore=shutil.ignore_patterns("_mut_*", "__pycache__", ".git"))
    with open(os.path.join(d, "interest.py"), "w", newline="\n") as f:
        f.write(BUGGY)
    rc, out = run(d)
    if rc == 0:
        fails.append("no test fails on the original code, so none of them reproduces the bug")
    elif "AssertionError" not in out:
        fails.append("the tests break on the original code without reaching an assertion: " + out[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
