import os
import re
import shutil
import subprocess
import sys

sys.path.insert(0, ".")
fails = []

CASES = (
    (0, 0), (1, 1), (9999, 9999), (10000, 9000), (10001, 9001), (10004, 9004),
    (10005, 9005), (10006, 9005), (20000, 18000), (123456, 111110),
)
try:
    import discount

    for total, want in CASES:
        got = discount.price_after_discount(total)
        if got != want:
            fails.append("price_after_discount(%d) is %r, want %d" % (total, got, want))
    try:
        discount.price_after_discount(-1)
        fails.append("a negative total was accepted")
    except ValueError:
        pass
except Exception as e:
    fails.append("discount.price_after_discount is not usable: %r" % (e,))

TEMPLATE = """def price_after_discount(total_cents):
    %(neg)s
    if total_cents >= %(thr)d:
        return %(calc)s
    return total_cents
"""
RAISE = "if total_cents < 0:\n        raise ValueError(total_cents)"
GOOD = dict(neg=RAISE, thr=10000, calc="(total_cents * 9 + 5) // 10")
MUTANTS = {
    "starts the discount one cent late": dict(GOOD, thr=10001),
    "starts the discount one cent early": dict(GOOD, thr=9999),
    "discounts every order": dict(GOOD, thr=0),
    "rounds down": dict(GOOD, calc="total_cents * 9 // 10"),
    "rounds halves down": dict(GOOD, calc="(total_cents * 9 + 4) // 10"),
    "rounds halves to even": dict(GOOD, calc="int(round(total_cents * 0.9))"),
    "gives no discount": dict(GOOD, calc="total_cents"),
    "accepts a negative total": dict(GOOD, neg="pass"),
}


def run(d):
    p = subprocess.run(
        [sys.executable, "-m", "unittest", "test_discount"],
        cwd=d, capture_output=True, encoding="utf-8", errors="replace", timeout=30,
    )
    return p.returncode, p.stderr


if not os.path.exists("test_discount.py"):
    fails.append("there is no test_discount.py")
else:
    rc, out = run(".")
    ran = re.search(r"Ran (\d+) test", out)
    if rc != 0:
        fails.append("the tests fail on the implementation: " + out[-300:])
    elif not ran or int(ran.group(1)) < 3:
        fails.append("fewer than three tests ran")
    else:
        for what, parts in MUTANTS.items():
            d = "_mut_%d" % len(os.listdir("."))
            shutil.copytree(".", d, ignore=shutil.ignore_patterns("_mut_*", "__pycache__", ".git"))
            with open(os.path.join(d, "discount.py"), "w", newline="\n", encoding="utf-8") as f:
                f.write(TEMPLATE % parts)
            if run(d)[0] == 0:
                fails.append("the tests still pass when the code " + what)
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
