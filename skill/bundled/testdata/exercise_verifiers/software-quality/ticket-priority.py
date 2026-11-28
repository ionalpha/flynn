import csv
import hashlib
import json
import os
import shutil
import subprocess
import sys

sys.path.insert(0, ".")
# The partner's job and its sample are the partner's, so they must be untouched.
THEIRS = {
    "partner_sync.py": "50f1e563c0799f3755d1098d1092d13584084441ae23725fd4bbb88902a64ded",
    "samples/partner_sample.csv": "464dd78109b4aa84e5b895e3057a4d9d80402cb65e295daf768123c09ecd908b",
}
OLD_HEADER = ["id", "title", "status", "opened"]
fails = []


def digest(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()


def count(path):
    return len(json.load(open(path, encoding="utf-8")))


for name, want in THEIRS.items():
    if not os.path.exists(name) or digest(name) != want:
        fails.append(name + " was edited or removed; it is the partner's copy, and their deployed job reads the file as it is")

# mkdtemp locks its directory to the creating user on recent Windows Pythons, which a confined process cannot write into.
tmp = os.path.join(os.getcwd(), ".verify-%d" % os.getpid())
os.makedirs(tmp)
try:
    import api
    import export
    import partner_sync

    store = os.path.join(tmp, "t.json")
    shutil.copy("data/tickets.json", store)

    status, body = api.create_ticket(store, {"title": "Cable cut", "priority": "high"})
    if status != 201 or body.get("priority") != "high":
        fails.append("create with priority high gave %r %r" % (status, body))
    status, body = api.create_ticket(store, {"title": "Old client, no priority"})
    if status != 201:
        fails.append("a client that sends no priority got %r; existing clients send none and must keep working" % (status,))
    n = count(store)
    for bad in ("p1", "", 5, ["high"], "urgent!"):
        status, body = api.create_ticket(store, {"title": "Bad priority", "priority": bad})
        if status != 400:
            fails.append("priority %r was answered %r; an unknown priority has to be refused, not stored or replaced" % (bad, status))
        if count(store) != n:
            fails.append("priority %r changed the stored tickets (%d to %d)" % (bad, n, count(store)))
            n = count(store)

    out = os.path.join(tmp, "tickets.csv")
    export.write(store, out)
    with open(out, newline="", encoding="utf-8") as f:
        got = list(csv.reader(f))
    if got[0][:4] != OLD_HEADER:
        fails.append("the first four export columns are %r; partners read them by position, so they stay %r" % (got[0][:4], OLD_HEADER))
    if len(got[0]) != 5 or got[0][4] != "priority":
        fails.append("the export header is %r; priority should be the one added column, at the end" % (got[0],))
    if len(got) != 6:
        fails.append("the export has %d rows for 5 tickets" % (len(got) - 1))
    else:
        for row, want in zip(got[1:4], ("1", "2", "3")):
            if row[0] != want or len(row) != 5 or not row[4]:
                fails.append("ticket %s, stored before priority existed, exports as %r; it needs a priority too" % (want, row))
        if got[4][4] != "high":
            fails.append("the ticket created with priority high exports %r" % (got[4],))
        if not got[5][4]:
            fails.append("the ticket created without priority exports an empty priority")
    legacy_open = [r for r in got[1:] if len(r) > 3 and r[2] == "open"]
    want_lines = ["%s\t%s\t%s" % (r[0], r[1], r[3]) for r in legacy_open]
    if len(got) == 6 and want_lines != list(partner_sync.lines(out)):
        fails.append("the partner job reads %r from the export; it should queue %r" % (list(partner_sync.lines(out)), want_lines))
    elif len(got) == 6 and [t[0] for t in map(lambda s: s.split("\t"), want_lines)] != ["1", "3", "4", "5"]:
        fails.append("the export's status and opened columns moved: %r" % (want_lines,))
except Exception as e:
    fails.append("the service could not be exercised: %r" % (e,))
finally:
    shutil.rmtree(tmp, ignore_errors=True)

p = subprocess.run([sys.executable, "-m", "unittest"], capture_output=True, encoding="utf-8", errors="replace", timeout=60)
if p.returncode != 0:
    fails.append("the project's own tests fail: " + (p.stdout + p.stderr)[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
