import json
import os
import re
import subprocess
import sys

fails = []
GOOD = {"A-1": 10.5, "C-3": 7.25, "F-6": 99.0}
# sku, line number of the three bad rows in feed.csv
BAD = (("B-2", 2), ("D-4", 4), ("E-5", 5))
OUT = "verify-prices.json"


def run(src):
    if os.path.exists(OUT):
        os.remove(OUT)
    p = subprocess.run([sys.executable, "importer.py", src, OUT], capture_output=True, encoding="utf-8", errors="replace", timeout=60)
    return p, p.stdout + p.stderr


p, said = run("feed.csv")
if not os.path.exists(OUT):
    fails.append("the import wrote no prices.json for a feed with three bad rows: %s" % said[-300:])
else:
    got = json.load(open(OUT, encoding="utf-8"))
    for sku, price in GOOD.items():
        if got.get(sku) != price:
            fails.append("%s should have loaded as %r, got %r" % (sku, price, got.get(sku)))
    for sku, _ in BAD:
        if sku in got:
            fails.append("%s is in prices.json with %r; a row that could not be read has no price, and a made-up one reaches the storefront" % (sku, got[sku]))
    if "Traceback" in said:
        fails.append("the import still crashes on the supplier's feed")
    for sku, line in BAD:
        named = sku in said or re.search(r"(line|row)\s*#?:?\s*%d\b" % line, said, re.I) is not None
        if not named:
            fails.append("nobody is told that %s (line %d) was dropped; the import's own output has to say so, not a debug log" % (sku, line))

with open("verify-clean.csv", "w", encoding="utf-8") as f:
    f.write("A-1,10.50\nC-3,7.25\n")
p, said = run("verify-clean.csv")
if p.returncode != 0 or "Traceback" in said:
    fails.append("a clean feed no longer imports: " + said[-300:])
elif json.load(open(OUT, encoding="utf-8")) != {"A-1": 10.5, "C-3": 7.25}:
    fails.append("a clean feed loaded different prices")
for name in (OUT, "verify-clean.csv"):
    if os.path.exists(name):
        os.remove(name)
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
