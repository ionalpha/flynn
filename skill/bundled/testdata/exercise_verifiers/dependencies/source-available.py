import ast
import hashlib
import os
import sys

sys.path.insert(0, ".")
fails = []


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()


if h("requirements.txt") != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855":
    fails.append("requirements.txt changed")
for root, dirs, files in os.walk("."):
    dirs[:] = [d for d in dirs if d not in (".git", "__pycache__", "third_party")]
    for name in files:
        if not name.endswith(".py"):
            continue
        p = os.path.join(root, name)
        src = open(p, encoding="utf-8").read()
        if "_fc_scan" in src:
            fails.append(p + " copies code from fastcsv, which is under the Business Source License")
        for node in ast.walk(ast.parse(src)):
            if isinstance(node, (ast.Import, ast.ImportFrom)):
                d = ast.dump(node)
                if "fastcsv" in d or "third_party" in d:
                    fails.append(p + " imports fastcsv, which is under the Business Source License")
try:
    import csvio

    rows = csvio.read_rows('id,name,note\n1,"Smith, Jo","said ""hi"""\n2,Lee,\n')
    want = [["id", "name", "note"], ["1", "Smith, Jo", 'said "hi"'], ["2", "Lee", ""]]
    if [list(r) for r in rows] != want:
        fails.append("read_rows gives %r" % (rows,))
except Exception as e:
    fails.append("csvio.read_rows is not usable: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
