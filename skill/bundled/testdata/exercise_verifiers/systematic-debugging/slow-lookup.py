import subprocess
import sys

fails = []
TIMED = r"""
import sys, time
sys.path.insert(0, ".")
from matcher import match
n = 20000
cat = [{"sku": "s" + str(i), "price": i} for i in range(n)]
cat += [{"sku": "s" + str(i), "price": -i} for i in range(0, n, 97)]
orders = [{"sku": "s" + str(n - 1 - i)} for i in range(n)] + [{"sku": "nope"}, {"sku": "s0"}]
want = [cat[n - 1 - i] for i in range(n)] + [None, cat[0]]
t = time.perf_counter()
got = match(orders, cat)
e = time.perf_counter() - t
if got != want:
    bad = next(i for i in range(len(want)) if i >= len(got) or got[i] != want[i])
    print("match is wrong at line", bad, "for", orders[bad], ": got", got[bad] if bad < len(got) else "nothing", file=sys.stderr)
    sys.exit(1)
if e >= 2.0:
    print("match took", round(e, 2), "seconds for", n, "lines", file=sys.stderr)
    sys.exit(1)
"""
try:
    p = subprocess.run([sys.executable, "-c", TIMED], capture_output=True, encoding="utf-8", errors="replace", timeout=20)
    if p.returncode != 0:
        fails.append(p.stderr.strip()[-400:])
except subprocess.TimeoutExpired:
    fails.append("match did not finish 20,000 lines in 20 seconds")
p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-q"], capture_output=True, encoding="utf-8", errors="replace", timeout=60)
if p.returncode != 0:
    fails.append("the tests fail: " + p.stderr[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
