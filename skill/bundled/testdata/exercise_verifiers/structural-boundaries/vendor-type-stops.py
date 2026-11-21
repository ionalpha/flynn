import ast
import hashlib
import os
import sys

sys.path.insert(0, ".")
fails = []


def digest(p):
    return hashlib.sha256(open(p, "rb").read().replace(b'\r\n', b'\n')).hexdigest()


try:
    if digest("acmepay/__init__.py") != "ab1e2e2ec4746315c32e2afdbdbd96ba3cdd1b0725b3d0742ed11612783f46e3":
        fails.append("acmepay is the vendor SDK and is not ours to change")
except OSError:
    fails.append("acmepay/__init__.py is missing")


def imports_vendor(tree):
    for node in ast.walk(tree):
        if isinstance(node, ast.Import) and any(a.name == "acmepay" or a.name.startswith("acmepay.") for a in node.names):
            return True
        if isinstance(node, ast.ImportFrom) and node.module and (node.module == "acmepay" or node.module.startswith("acmepay.")):
            return True
    return False


for root, dirs, files in os.walk("."):
    dirs[:] = [d for d in dirs if d not in (".git", "__pycache__", "acmepay") and not d.startswith("_mut")]
    for name in files:
        p = os.path.normpath(os.path.join(root, name))
        if not name.endswith(".py") or name.startswith("test") or p == "payments.py":
            continue
        if imports_vendor(ast.parse(open(p, encoding="utf-8").read())):
            fails.append(p + " imports acmepay; the SDK's types stop at payments.py")


def vendor_typed(value, depth=0, seen=None):
    seen = set() if seen is None else seen
    if id(value) in seen or depth > 6:
        return False
    seen.add(id(value))
    if type(value).__module__.split(".")[0] == "acmepay":
        return True
    kids = []
    if isinstance(value, dict):
        kids = list(value.keys()) + list(value.values())
    elif isinstance(value, (list, tuple, set, frozenset)):
        kids = list(value)
    else:
        if hasattr(value, "__dict__"):
            kids = list(vars(value).values())
    return any(vendor_typed(k, depth + 1, seen) for k in kids)


try:
    import payments
    import reports

    for c in payments.list_charges():
        if vendor_typed(c):
            fails.append("list_charges hands out an acmepay object (in %r); translate what the report reads" % (c,))
            break
    lines = reports.monthly_lines()
    want = [("ch_1", "visa"), ("ch_2", "mastercard"), ("ch_3", "amex")]
    if len(lines) != 3:
        fails.append("the report has %d lines, want 3: %r" % (len(lines), lines))
    for line, (cid, brand) in zip(lines, want):
        if cid not in line or brand not in line.lower():
            fails.append("the line for %s does not show the brand %s: %r" % (cid, brand, line))
    if lines and "42.00" not in lines[0]:
        fails.append("the first line lost its amount: %r" % (lines[0],))
except Exception as e:
    fails.append("the report could not be exercised: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
