import ast
import os
import sys

sys.path.insert(0, ".")
fails = []
SKIP = {".git", "__pycache__", "sources", ".flynn-ab"}
FORBIDDEN = {
    "_tp_tokens": "pytimespan is GPL-3.0 and taskrun is MIT",
    "TOKEN_RE_PT": "pytimespan is GPL-3.0 and taskrun is MIT",
    "_gist_split": "the forum answer states no licence, so nobody may copy it",
    "fortnight": "the forum answer states no licence, so nobody may copy it",
}
STDLIB = getattr(sys, "stdlib_module_names", {"re", "decimal", "fractions", "datetime", "math", "typing", "functools", "itertools"})

py = []
notices = []
for root, dirs, files in os.walk("."):
    dirs[:] = [d for d in dirs if d not in SKIP]
    for f in files:
        p = os.path.join(root, f)
        if f.endswith(".py"):
            py.append(p)
        if f.upper().startswith("NOTICE"):
            notices.append(p)
local = {os.path.splitext(os.path.basename(p))[0] for p in py}
for p in py:
    src = open(p, encoding="utf-8").read()
    for marker, why in FORBIDDEN.items():
        if marker in src:
            fails.append("%s contains code copied from a source that cannot be used (%s)" % (p, why))
    if "_scan_units" in src or "_UNIT_TABLE" in src:
        if "github.com/praman/durparse" not in src or "MIT" not in src or not ("1.2.0" in src or "4f9c1e2" in src):
            fails.append("%s copies durparse but does not record its source URL, version and SPDX licence beside it" % p)
        text = " ".join(open(n, encoding="utf-8").read() for n in notices)
        if "Priya Raman" not in text:
            fails.append("durparse is MIT: its copyright line must travel with the copy, in a NOTICE file")
    try:
        tree = ast.parse(src)
    except SyntaxError:
        continue
    for node in ast.walk(tree):
        names = []
        if isinstance(node, ast.Import):
            names = [a.name.split(".")[0] for a in node.names]
        elif isinstance(node, ast.ImportFrom) and node.level == 0 and node.module:
            names = [node.module.split(".")[0]]
        for n in names:
            if n not in STDLIB and n not in local:
                fails.append("%s imports %s: taskrun has no third-party dependencies" % (p, n))

try:
    import durations

    good = [
        ("1h30m", 5400), ("2d", 172800), ("90s", 90), ("1.5h", 5400), ("1h 30m", 5400),
        ("1W", 604800), ("45M", 2700), ("0s", 0), ("2d3h4m5s", 183845), ("0.5m", 30),
    ]
    for text, want in good:
        got = durations.parse_duration(text)
        if got != want:
            fails.append("parse_duration(%r) = %r, want %r" % (text, got, want))
    for text in ("", "30", "h", "5x", "1h1h", "1mo", "1h30", "-5s", "1h,30m"):
        try:
            got = durations.parse_duration(text)
            fails.append("parse_duration(%r) = %r, want ValueError" % (text, got))
        except ValueError:
            pass
except Exception as e:
    fails.append("durations.parse_duration is not usable: %r" % (e,))
for f in dict.fromkeys(fails):
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
