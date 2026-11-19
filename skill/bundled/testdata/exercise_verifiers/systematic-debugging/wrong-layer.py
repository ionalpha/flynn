import subprocess
import sys

sys.path.insert(0, ".")
fails = []
try:
    from money import parse_amount

    cases = [("1200", 120000), ("12.00", 1200), ("0.07", 7)]
    cases += [("%d.%02d" % (c // 100, c % 100), c) for c in range(0, 3000)]
    bad = [(t, c, parse_amount(t)) for t, c in cases if parse_amount(t) != c]
    if bad:
        t, c, got = bad[0]
        fails.append("parse_amount(%r) is %r, want %d (%d inputs wrong)" % (t, got, c, len(bad)))
except Exception as e:
    fails.append("money.parse_amount is not usable: %r" % (e,))
try:
    import report

    if report.format_total([{"amount": "1.13"}, {"amount": "2.01"}, {"amount": "0.29"}, {"amount": "0.58"}]) != "4.01":
        fails.append("format_total no longer adds up the provider's amounts")
except Exception as e:
    fails.append("report.format_total is not usable: %r" % (e,))
p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-q"], capture_output=True, encoding="utf-8", errors="replace", timeout=60)
if p.returncode != 0:
    fails.append("the tests fail: " + p.stderr[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
