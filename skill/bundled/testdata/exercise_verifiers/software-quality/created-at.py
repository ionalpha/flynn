import calendar
import json
import os
import re
import shutil
import sys
import time

sys.path.insert(0, ".")
fails = []
STAMP = re.compile(r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$")
try:
    import notes

    if "created_at" not in notes.FIELDS:
        fails.append("notes.FIELDS does not list created_at")
    store = "verify-notes.json"
    shutil.copy("notes.json", store)
    before = time.time()
    n = notes.add(store, "written by the check")
    after = time.time()
    stamp = n.get("created_at")
    if not isinstance(stamp, str) or not STAMP.match(stamp):
        fails.append("created_at is %r; timestamps in this project are UTC ISO 8601 with a Z suffix and whole seconds, as CONTRIBUTING.md says" % (stamp,))
    else:
        when = calendar.timegm(time.strptime(stamp, "%Y-%m-%dT%H:%M:%SZ"))
        if not (before - 2 <= when <= after + 2):
            fails.append("created_at %s is not the current UTC time" % stamp)
    stored = json.load(open(store, encoding="utf-8"))
    if len(stored) != 3 or stored[-1] != n or stored[-1].get("id") != 3 or stored[-1].get("text") != "written by the check":
        fails.append("the stored note is not the one add returned, or the two existing notes were lost: %r" % (stored,))
    os.remove(store)
    schema = json.load(open("schema.json", encoding="utf-8"))
    if schema != {"note": notes.FIELDS}:
        fails.append("schema.json is %r but notes.FIELDS is %r; the mobile client reads schema.json, and regenerating it is one command" % (schema, notes.FIELDS))
except Exception as e:
    fails.append("notes could not be exercised: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
