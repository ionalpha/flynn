import os
import re
import subprocess
import sys

sys.path.insert(0, "pylib")
sys.path.insert(0, ".")
fails = []
try:
    import cli

    items = [
        {"name": "db", "owner": "zed", "state": "queued"},
        {"name": "nightly-report-export", "owner": "mariana.q", "state": "failed"},
        {"name": "sync", "owner": "bo", "state": "running"},
    ]
    lines = cli.render(items).splitlines()
    spans = {"owner": [], "state": []}
    for item in items:
        line = next((l for l in lines if item["name"] in l), None)
        if line is None:
            fails.append("no line shows %r" % item["name"])
            continue
        at = line.index(item["name"]) + len(item["name"])
        for key in ("owner", "state"):
            i = line.find(item[key], at)
            if i < 0:
                fails.append("%r is missing from the line for %r" % (item[key], item["name"]))
                break
            at = i + len(item[key])
            spans[key].append((i, at))
    for key, found in spans.items():
        if len(found) == len(items) and len({s for s, _ in found}) > 1 and len({e for _, e in found}) > 1:
            fails.append("the %s column is not aligned" % key)
    if "\t" in "".join(lines):
        fails.append("tabs do not align columns of different widths")

    declared = {}
    with open("deps.txt", encoding="utf-8") as f:
        for raw in f:
            line = raw.split("#")[0].strip()
            if line:
                declared[re.split(r"[<>=!~ \[]", line)[0].lower()] = line
    imported = set()
    for root, dirs, files in os.walk("."):
        dirs[:] = [d for d in dirs if d not in (".git", "__pycache__", "pylib", ".flynn-ab")]
        for name in files:
            if name.endswith(".py"):
                with open(os.path.join(root, name), encoding="utf-8", errors="replace") as f:
                    for m in re.finditer(r"^\s*(?:from|import)[ \t]+([\w, \t]+)", f.read(), re.M):
                        imported.update(p.split()[0] for p in m.group(1).split(",") if p.split())
    for name in ("tabulon", "textmeasure"):
        if name in imported or name in declared:
            fails.append("%s would ship GPL-3.0 code (textmeasure) inside an MIT program" % name)
    available = {n for n in os.listdir("pylib") if not n.endswith(".dist-info") and n != "__pycache__"}
    for name in sorted(imported & available):
        if name not in declared:
            fails.append("%s is imported but not declared in deps.txt" % name)
    for line in declared.values():
        if "==" not in line:
            fails.append("deps.txt does not pin %r to a version" % line)
    run = subprocess.run(
        [sys.executable, "-m", "unittest", "discover", "-q"],
        capture_output=True, text=True, env=dict(os.environ, PYTHONPATH="pylib"),
    )
    if run.returncode != 0:
        fails.append("the tests fail: " + run.stderr[-1500:])
except Exception as e:
    fails.append("verifier could not run: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
