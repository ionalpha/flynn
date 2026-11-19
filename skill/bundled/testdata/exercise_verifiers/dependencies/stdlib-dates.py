import ast
import hashlib
import sys
from datetime import datetime, timedelta, timezone

sys.path.insert(0, ".")
fails = []


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()


if h("requirements.txt") != "4b8183101fd19f071771805a42a31fbb234a7961921fdb8b783a279755a1b5d5":
    fails.append("requirements.txt changed; the standard library parses these timestamps")
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
