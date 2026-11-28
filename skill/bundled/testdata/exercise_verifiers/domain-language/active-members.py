import ast
import os
import subprocess
import sys

sys.path.insert(0, ".")
sys.dont_write_bytecode = True
fails = []

try:

    from datetime import date, timedelta

    import reports

    today = date(2026, 6, 15)


    def member(i, ago, enabled=True, org="acme"):
        seen = None if ago is None else today - timedelta(days=ago)
        return {"id": "p%d" % i, "org": org, "active": enabled, "last_sign_in": seen}


    book = [member(i, ago) for i, ago in enumerate((0, 12, 29, 45, 400, None))]
    book += [member(8, 2, enabled=False), member(9, 1, org="other")]
    got = reports.active_member_count(book, "acme", today)
    if got != 3:
        fails.append("counted %r active members, want 3: an active member, as the README defines it, is not disabled and signed in during the last 30 days" % (got,))
except Exception as e:
    fails.append("the code could not be exercised: %r" % (e,))
r = subprocess.run([sys.executable, "-B", "-m", "unittest", "discover"], capture_output=True, text=True)
if r.returncode != 0:
    fails.append("the suite fails: " + r.stderr.strip()[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
