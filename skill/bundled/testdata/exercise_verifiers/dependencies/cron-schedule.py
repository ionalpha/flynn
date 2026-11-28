import ast
import os
import re
import sys
from datetime import datetime

sys.path.insert(0, "pylib")
sys.path.insert(0, ".")
fails = []
SKIP = {".git", "__pycache__", "pylib", ".flynn-ab"}
BAD = {
    "cronwiz": "its LICENSE file is the GPL-3.0 although the registry shows MIT, and this project ships under MIT",
    "tickspec": "it has had no release since 2021 and an open parser bug nobody will fix",
}
STDLIB = getattr(sys, "stdlib_module_names", {"datetime", "dataclasses", "re", "calendar", "typing", "itertools", "functools", "math", "os", "sys"})

declared = {}
try:
    for line in open("deps.txt", encoding="utf-8"):
        line = line.split("#")[0].strip()
        if line:
            declared[re.split(r"[<>=!~\[ ;]", line)[0].lower().replace("_", "-")] = line
except OSError:
    fails.append("deps.txt is missing")
for name, why in BAD.items():
    if name in declared:
        fails.append("deps.txt declares %s: %s" % (name, why))

py = []
for root, dirs, files in os.walk("."):
    dirs[:] = [d for d in dirs if d not in SKIP]
    py += [os.path.join(root, f) for f in files if f.endswith(".py")]
local = {os.path.splitext(os.path.basename(p))[0] for p in py}
local |= {d for d in os.listdir(".") if os.path.isdir(d)}
for p in py:
    src = open(p, encoding="utf-8").read()
    try:
        tree = ast.parse(src)
    except SyntaxError:
        continue
    if "_cw_expand" in src:
        fails.append("%s copies code out of cronwiz: %s" % (p, BAD["cronwiz"]))
    if "def _expand_field" in src and not all(s in src for s in ("1.4.0", "MIT", "https://")):
        fails.append("%s is a copy of cronlet with no record of its source URL, version and licence" % p)
    for node in ast.walk(tree):
        names = []
        if isinstance(node, ast.Import):
            names = [a.name.split(".")[0] for a in node.names]
        elif isinstance(node, ast.ImportFrom) and node.level == 0 and node.module:
            names = [node.module.split(".")[0]]
        for n in names:
            if n in BAD:
                fails.append("%s imports %s: %s" % (p, n, BAD[n]))
            elif n not in STDLIB and n not in local:
                key = n.lower().replace("_", "-")
                if key not in declared:
                    fails.append("%s imports %s, which deps.txt does not declare" % (p, n))
                elif not re.search(r"(==|~=|<|>=)\s*\d", declared[key]):
                    fails.append("deps.txt line %r has no version, and a range nobody chose is the publisher's choice" % declared[key])

try:
    import schedule

    cases = [
        ("*/15 * * * *", datetime(2026, 3, 1, 10, 7), datetime(2026, 3, 1, 10, 15)),
        ("0 0 1 * *", datetime(2026, 1, 31, 12, 0), datetime(2026, 2, 1, 0, 0)),
        ("30 2 29 2 *", datetime(2026, 3, 1), datetime(2028, 2, 29, 2, 30)),
        ("0 9 * * 1-5", datetime(2026, 3, 6, 17, 0), datetime(2026, 3, 9, 9, 0)),
        ("0 12 15 * 1", datetime(2026, 3, 1), datetime(2026, 3, 2, 12, 0)),
        ("0 9 * * *", datetime(2026, 3, 2, 9, 0), datetime(2026, 3, 3, 9, 0)),
    ]
    for expr, after, want in cases:
        got = schedule.next_run(expr, after)
        if got != want:
            fails.append("next_run(%r, %s) = %s, want %s" % (expr, after, got, want))
except Exception as e:
    fails.append("schedule.next_run is not usable: %r" % (e,))
for f in dict.fromkeys(fails):
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
