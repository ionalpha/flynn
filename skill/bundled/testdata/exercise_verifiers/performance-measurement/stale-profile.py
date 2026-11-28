import hashlib
import random
import subprocess
import sys

sys.path.insert(0, ".")
fails = []


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()[:16]


for name, want in (("bench.py", "8412cadedb8a17f3"), ("vendor/fmt.py", "069568c34de0b77c")):
    try:
        if h(name) != want:
            fails.append(name + " was edited")
    except OSError:
        fails.append(name + " is gone")
r = subprocess.run([sys.executable, "test_report.py"], capture_output=True, text=True)
if r.returncode != 0 or "test_repeats_are_dropped_and_order_is_kept" not in open("test_report.py").read():
    fails.append("test_report.py no longer passes as it was")

EQ = [0]


class S(str):
    __hash__ = str.__hash__

    def __eq__(self, other):
        EQ[0] += 1
        return str.__eq__(self, other)


def rows_a():
    rnd = random.Random(4)
    base = [("acct-%d" % (i % 700), "2024-03-%02d" % (i % 28 + 1), str(i % 97)) for i in range(1000)]
    return base + [rnd.choice(base) for _ in range(1000)]


def rows_b():
    odd = [("x|1", "2", "5"), ("x", "1|2", "5"), ("x", "1", "2|5"), ("x", "1", 5), ("x", "1", "5"), ("x", "1", 5)]
    return odd + [("u%d" % i, "2024-04-%02d" % (i % 28 + 1), str(i)) for i in range(1500)] + odd[:3]


def ref(rows):
    seen, out = set(), []
    for a, d, m in rows:
        if (a, d, m) not in seen:
            seen.add((a, d, m))
            out.append("%-12s %s %10s" % (a, d, m))
    return out


try:
    import report

    for label, rows in (("the repeated-heavy export", rows_a()), ("a mostly distinct export with awkward keys", rows_b())):
        data = [{"account": S(a), "day": d, "amount": m} for a, d, m in rows]
        EQ[0] = 0
        got = report.build(data)
        if got != ref(rows):
            fails.append("%s: the report lines differ from the old output (%d lines, want %d)" % (label, len(got), len(ref(rows))))
        if EQ[0] > 60 * len(rows):
            fails.append("%s: %d key comparisons for %d rows; the repeat check still scales with the square" % (label, EQ[0], len(rows)))
except Exception as e:
    fails.append("report.build is not usable: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
