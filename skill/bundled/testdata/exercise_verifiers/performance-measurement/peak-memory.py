import os
import random
import shutil
import subprocess
import sys
import tracemalloc

sys.path.insert(0, ".")
fails = []


def scratch():
    d = os.path.abspath("_mut_scratch")
    os.makedirs(d, exist_ok=True)
    return d

r = subprocess.run([sys.executable, "test_summary.py"], capture_output=True, text=True)
if r.returncode != 0 or "test_counts_errors_and_p95_per_endpoint" not in open("test_summary.py").read():
    fails.append("test_summary.py no longer passes as it was")


def make(path, n, tail, seed):
    rnd = random.Random(seed)
    with open(path, "w", newline="\n") as f:
        for i in range(n):
            ep = rnd.choice(["items", "items", "orders", "carts"])
            f.write("2024-05-01T12:%02d:%02dZ GET /api/%s/%d?q=%d %d %d %s\n" % (
                i // 60 % 60, i % 60, ep, rnd.randrange(50), i, rnd.choice([200, 200, 404, 500, 503]), rnd.randrange(20, 900), tail))
            if i % 997 == 0:
                f.write("garbage %d\n" % i)
                f.write("2024-05-01T12:00:00Z GET /api/items/1 20x 30 ua\n")


def ref(path):
    per = {}
    for line in open(path, encoding="utf-8").read().splitlines():
        p = line.split(" ", 5)
        if len(p) < 5 or not (p[3].isdigit() and p[4].isdigit()):
            continue
        ep = "/".join(p[2].split("?")[0].split("/")[:3])
        per.setdefault(ep, []).append((int(p[3]), int(p[4])))
    out = {}
    for ep, v in per.items():
        ms = sorted(m for _, m in v)
        out[ep] = {"count": len(v), "errors": sum(1 for s, _ in v if s >= 500), "p95": ms[int(0.95 * (len(ms) - 1))]}
    return out


d = scratch()
try:
    import summary

    cases = (
        ("the shipped sample", "data/sample.log", None),
        ("a day of short lines", os.path.join(d, "a.log"), (60000, "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36", 5, 0.6)),
        ("a day of long lines", os.path.join(d, "b.log"), (12000, "x" * 2000, 8, 0.1)),
    )
    for label, path, spec in cases:
        if spec:
            make(path, spec[0], spec[1], spec[2])
        want = ref(path)
        tracemalloc.start()
        try:
            got = summary.summarise(path)
            peak = tracemalloc.get_traced_memory()[1]
        finally:
            tracemalloc.stop()
        if got != want:
            fails.append("%s: the summary differs from the exact answer" % label)
        if spec and peak > spec[3] * os.path.getsize(path):
            fails.append("%s: peak memory %.1f MB for a %.1f MB log; it still holds the log in memory" % (
                label, peak / 1e6, os.path.getsize(path) / 1e6))
except Exception as e:
    fails.append("summary.summarise is not usable: %r" % (e,))
shutil.rmtree(d, ignore_errors=True)
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
