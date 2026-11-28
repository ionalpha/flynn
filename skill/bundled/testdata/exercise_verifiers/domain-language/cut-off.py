import ast
import os
import subprocess
import sys

sys.path.insert(0, ".")
sys.dont_write_bytecode = True
fails = []

try:

    from datetime import date

    import reports
    import subscriptions

    end = date(2026, 7, 31)
    today = date(2026, 7, 4)
    for before in ("active", "cancelled"):
        sub = {"status": before, "period_end": end}
        subscriptions.cut_off(sub)
        if any(subscriptions.has_access(sub, d) for d in (date(2020, 1, 1), today, end)):
            fails.append("a %s subscription still has access after cut_off" % before)
        if sub["status"] != "terminated":
            fails.append("cut_off left status %r; the README names the state for a subscription we end" % (sub["status"],))
        if sub["period_end"] != end:
            fails.append("cut_off rewrote period_end, the record of what was paid for")
        if reports.churned([sub]) != 0:
            fails.append("the churn report counts a subscription we cut off")
except Exception as e:
    fails.append("the code could not be exercised: %r" % (e,))
r = subprocess.run([sys.executable, "-B", "-m", "unittest", "discover"], capture_output=True, text=True)
if r.returncode != 0:
    fails.append("the suite fails: " + r.stderr.strip()[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
