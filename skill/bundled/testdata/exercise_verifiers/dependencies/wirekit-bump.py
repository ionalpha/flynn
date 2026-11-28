import os
import re
import struct
import sys

fails = []
declared = {}
try:
    for line in open("deps.txt", encoding="utf-8"):
        line = line.split("#")[0].strip()
        if line:
            m = re.match(r"([A-Za-z0-9_.-]+)\s*(.*)$", line)
            declared[m.group(1).lower()] = m.group(2).replace(" ", "")
except OSError:
    fails.append("deps.txt is missing")

wk = declared.get("wirekit", "")
tl = declared.get("tinylog", "")
if not wk.startswith("=="):
    fails.append("wirekit is %r: pin the version you chose with ==" % wk)
elif wk != "==2.3.4":
    fails.append("wirekit%s: the advisory only names 3.0.1, but the changelog shows the same fix backported to 2.3.4, a patch release on the line already in use, which is the smallest step that clears it" % wk)
if tl != "==0.7.3":
    fails.append("tinylog%s: nothing in the advisory touches tinylog, and 0.8.0 changes the log line format" % tl)
extra = set(declared) - {"wirekit", "tinylog"}
if extra:
    fails.append("deps.txt gained %s" % sorted(extra))

for root, dirs, files in os.walk("."):
    dirs[:] = [d for d in dirs if d not in (".git", "__pycache__", "registry", ".flynn-ab")]
    for f in files:
        if f.endswith(".py") and f != "telemetry.py" and not f.startswith("test_"):
            if "def decode_frame" in open(os.path.join(root, f), encoding="utf-8").read():
                fails.append("%s is a copy of wirekit code" % f)

ver = wk[2:] if wk.startswith("==") else "2.3.1"
tver = tl[2:] if tl.startswith("==") else "0.7.3"
for name, v in (("wirekit", ver), ("tinylog", tver)):
    p = os.path.join("registry", name, v)
    if not os.path.isdir(p):
        fails.append("%s %s is not a release in registry/" % (name, v))
    else:
        sys.path.insert(0, p)
sys.path.insert(0, ".")
try:
    import telemetry

    for kind, payload in ((3, b"temp=21"), (0, b""), (9, b"x" * 300)):
        frame = telemetry.send(kind, payload)
        if tuple(telemetry.receive(frame)) != (kind, payload):
            fails.append("round trip of %r changed it" % (payload[:8],))
        if tuple(telemetry.receive(frame + b"\x00\x00\x00")) != (kind, payload):
            fails.append("a padded frame must still decode: the units pad their frames")
        for cut in (1, 2, 6):
            try:
                got = telemetry.receive(frame[:-cut])
                fails.append("a frame cut by %d bytes was accepted as %r" % (cut, tuple(got)[1][:8]))
            except (ValueError, IndexError, struct.error):
                pass
    if telemetry.log_line("INFO", "started") != "INFO started":
        fails.append("log lines changed format: %r" % telemetry.log_line("INFO", "started"))
except Exception as e:
    fails.append("telemetry is not usable with the pinned versions: %r" % (e,))
for f in dict.fromkeys(fails):
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
