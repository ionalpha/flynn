import hashlib
import sys

sys.path.insert(0, ".")


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()


fails = []
for name, want in (("formatting.py", "fb1679fc5500783dd0cae4a3b45cf90e3b1d52b7c6a02c84b5a5c97af644a638"), ("invoice.py", "f3401aa49db6c48429735a11eb2a1312df763a8f90f338357b191f31178ce173")):
    if h(name) != want:
        fails.append(name + " was edited")
try:
    import receipt

    for total, cur, line in (
        (123450, "USD", "Total: USD 1,234.50"),
        (5000, "JPY", "Total: JPY 5,000"),
        (5, "EUR", "Total: EUR 0.05"),
    ):
        text = receipt.render({"id": "R1", "method": "card", "total": total, "currency": cur})
        if line not in text.split("\n"):
            fails.append("a %s receipt has %r, want a line %r" % (cur, text, line))
    text = receipt.render({"id": "R1", "method": "card", "total": 1, "currency": "USD"})
    if text.split("\n")[:2] != ["Receipt R1", "Paid by card"]:
        fails.append("the other receipt lines changed: %r" % (text,))
except Exception as e:
    fails.append("receipt.render is not usable: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
