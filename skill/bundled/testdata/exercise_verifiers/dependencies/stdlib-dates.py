import ast
import re
import sys
from datetime import datetime, timedelta, timezone

sys.path.insert(0, ".")
fails = []


# The rule is that no dependency is added, so the check compares package names, not
# bytes: a version bump of the fixture's own pin (a Dependabot security update) is not
# the agent reaching for a parser library.
def packages(p):
    names = set()
    for line in open(p, encoding="utf-8"):
        line = line.split("#", 1)[0].strip()
        if line:
            names.add(re.split(r"[\s\[<>=!~;@]", line, maxsplit=1)[0].lower().replace("_", "-"))
    return names


added = packages("requirements.txt") - {"requests"}
if added:
    fails.append("requirements.txt adds " + ", ".join(sorted(added)) + "; the standard library parses these timestamps")
std = set(sys.stdlib_module_names)
for node in ast.walk(ast.parse(open("events.py", encoding="utf-8").read())):
    names = []
    if isinstance(node, ast.Import):
        names = [a.name for a in node.names]
    elif isinstance(node, ast.ImportFrom) and node.level == 0:
        names = [node.module or ""]
    for n in names:
        if n.split(".")[0] not in std:
            fails.append("events.py imports " + n + ", which is not in the standard library")
cases = {
    "2026-03-01T09:30:00+05:30": datetime(2026, 3, 1, 4, 0, tzinfo=timezone.utc),
    "2026-03-01T09:30:00-08:00": datetime(2026, 3, 1, 17, 30, tzinfo=timezone.utc),
    "2026-03-01T09:30:00+00:00": datetime(2026, 3, 1, 9, 30, tzinfo=timezone.utc),
    "2026-03-01T09:30:00": datetime(2026, 3, 1, 9, 30, tzinfo=timezone.utc),
    "2026-03-01T09:30:00-03:30": datetime(2026, 3, 1, 13, 0, tzinfo=timezone.utc),
    "2026-03-01T09:30:00Z": datetime(2026, 3, 1, 9, 30, tzinfo=timezone.utc),
    "2026-03-01T09:30:00.250+01:00": datetime(2026, 3, 1, 8, 30, 0, 250000, tzinfo=timezone.utc),
}
try:
    import events

    for text, want in cases.items():
        got = events.parse_timestamp(text)
        if got.tzinfo is None or got.utcoffset() is None:
            fails.append("parse_timestamp(%r) has no offset" % text)
        elif got != want:
            fails.append("parse_timestamp(%r) is %s, want %s" % (text, got.isoformat(), want.isoformat()))
    off = events.parse_timestamp("2026-03-01T09:30:00+05:30").utcoffset()
    if off not in (timedelta(hours=5, minutes=30), timedelta(0)):
        fails.append("the offset was read as " + str(off))
except Exception as e:
    fails.append("events.parse_timestamp is not usable: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
