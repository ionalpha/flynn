"""Fail when a config file is not UTF-8 or has a tab in it."""
import pathlib
import sys

bad = []
for path in sorted(pathlib.Path("config").glob("*.ini")):
    raw = path.read_bytes()
    try:
        raw.decode("utf-8")
    except UnicodeDecodeError:
        bad.append("%s is not UTF-8" % path)
    if b"\t" in raw:
        bad.append("%s has a tab" % path)
for line in bad:
    print(line, file=sys.stderr)
sys.exit(1 if bad else 0)
