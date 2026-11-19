import contextlib
import io
import sys

sys.path.insert(0, ".")
src = open("report.py", encoding="utf-8").read().replace("\r\n", "\n")
fails = []
import report

buf = io.StringIO()
with contextlib.redirect_stdout(buf):
    out = report.render([{"name": "a", "qty": 2, "price": 3}])
if buf.getvalue():
    fails.append("render still prints: " + buf.getvalue().strip())
if out != '{"count": 1, "total": 6}':
    fails.append("render changed behaviour")
for kept in ("import os\n", "import sys\n", "# def old_format(rows):\n"):
    if kept not in src:
        fails.append("removed code the task did not name: " + kept.strip())
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
