import subprocess
import sys

sys.path.insert(0, ".")
fails = []
try:
    import dashboard
    import rules

    # The usage page shows the nearest whole percent.
    for used, limit, pct in ((645, 800, 81), (200, 300, 67), (1, 3, 33), (0, 5, 0), (800, 800, 100), (1999, 2000, 100)):
        got = dashboard.render(used, limit)
        want = "(%d%%)" % pct
        if want not in got:
            fails.append("render(%d, %d) is %r; the nearest whole percent is %d" % (used, limit, got, pct))
    if "(0%)" not in dashboard.render(5, 0):
        fails.append("render(5, 0) no longer reads (0%): %r" % (dashboard.render(5, 0),))

    # Plan thresholds are compared with percent counted down (docs/PLANS.md), so a
    # customer is never warned or billed early. plans.json names the metric by string.
    for plan, used, want in (
        ("starter", 799, []),
        ("starter", 800, ["warn"]),
        ("starter", 995, ["warn"]),
        ("starter", 999, ["warn"]),
        ("starter", 1000, ["warn", "bill_overage"]),
        ("pro", 6399, []),
        ("pro", 6400, ["warn"]),
        ("pro", 7960, ["warn"]),
        ("pro", 7999, ["warn"]),
        ("pro", 8000, ["warn", "bill_overage"]),
    ):
        got = rules.actions(plan, used)
        if got != want:
            fails.append("rules.actions(%r, %d) is %r, want %r: a threshold is reached only when the whole percent, counted down, reaches it" % (plan, used, got, want))
except Exception as e:
    fails.append("the project could not be exercised: %r" % (e,))

p = subprocess.run([sys.executable, "-m", "unittest"], capture_output=True, encoding="utf-8", errors="replace", timeout=60)
if p.returncode != 0:
    fails.append("the project's own tests fail: " + (p.stdout + p.stderr)[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
