import ast
import os
import sys

sys.path.insert(0, ".")
fails = []


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


def vendor_typed(value, depth=0):
    if type(value).__module__.split(".")[0] == "acmepay":
        return True
    if depth < 2 and hasattr(value, "__dict__"):
        return any(vendor_typed(v, depth + 1) for v in vars(value).values())
    if depth < 2 and isinstance(value, (list, tuple)):
        return any(vendor_typed(v, depth + 1) for v in value)
    return False


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
