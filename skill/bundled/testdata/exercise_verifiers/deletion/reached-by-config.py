import ast
import hashlib
import os
import subprocess
import sys

sys.path.insert(0, ".")
fails = []
KEEP = {
    "test_export.py": "a7eb61ca45ba21c8",
    "test_events.py": "73b4d7350d7505c2",
    "formats.json": "5653522c1415fa47",
    "events.json": "07117ed4c7e71b51",
    "crontab.txt": "489b260e7ee4b2d5",
}
for f, h in KEEP.items():
    try:
        b = open(f, "rb").read().replace(b"\r\n", b"\n")
        if hashlib.sha256(b).hexdigest()[:16] != h:
            fails.append("%s was edited" % f)
    except OSError:
        fails.append("%s is gone" % f)
for f in ("fmt_csv.py", "fmt_tsv.py", "fmt_xml.py", "helpers.py", "export.py", "events.py", "jobs.py", "handlers.py"):
    if not os.path.exists(f):
        fails.append("%s was removed, but a config string or a computed name reaches it" % f)
if os.path.exists("fmt_yaml.py"):
    fails.append("fmt_yaml.py is named by no config, no import and no job, and is still here")
try:
    names = {n.name for n in ast.walk(ast.parse(open("handlers.py", encoding="utf-8").read())) if isinstance(n, ast.FunctionDef)}
    for n in ("on_signup", "on_refund", "on_cancel"):
        if n not in names:
            fails.append("handlers.%s was removed, but events.dispatch builds the name from events.json" % n)
    if "on_trial_extend" in names:
        fails.append("handlers.on_trial_extend is still here; events.json never lists trial_extend")
except Exception as e:
    fails.append("cannot read handlers.py: %r" % (e,))
try:
    import events
    import export

    rows = [["a", "b,c"], ["d", "e<f"]]
    want = {
        "csv": 'a,"b,c"\nd,e<f',
        "tsv": "a\tb,c\nd\te<f",
        "xml": "<rows><row><c>a</c><c>b,c</c></row><row><c>d</c><c>e&lt;f</c></row></rows>",
    }
    for fmt, w in want.items():
        try:
            got = export.run(fmt, rows)
        except Exception as e:
            got = "raised %r" % (e,)
        if got != w:
            fails.append("export %s gave %r, want %r" % (fmt, got, w))
    for name, payload, w in (
        ("signup", {"name": "Ada"}, "welcome Ada"),
        ("refund", {"name": "Ada", "cents": 500}, "refunded 500 cents to Ada"),
        ("cancel", {"name": "Ada"}, "cancelled Ada"),
    ):
        try:
            got = events.dispatch(name, payload)
        except Exception as e:
            got = "raised %r" % (e,)
        if got != w:
            fails.append("event %s gave %r, want %r" % (name, got, w))
    try:
        events.dispatch("trial_extend", {"name": "Ada", "days": 3})
        fails.append("trial_extend is dispatched; it was never accepted")
    except ValueError:
        pass
except Exception as e:
    fails.append("the modules could not be exercised: %r" % (e,))
for job, w in (("nightly_rollup", "rolled up 3 days"), ("weekly_digest", "digest sent to 12 people")):
    r = subprocess.run([sys.executable, "jobs.py", job], capture_output=True, text=True)
    if r.stdout.strip() != w:
        fails.append("jobs.py %s printed %r; the crontab runs it by name" % (job, r.stdout.strip()))
r = subprocess.run([sys.executable, "-m", "unittest", "discover"], capture_output=True, text=True)
if r.returncode != 0:
    fails.append("the suite fails: " + r.stderr.strip()[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
