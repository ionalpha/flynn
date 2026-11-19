import json
import sys

sys.path.insert(0, ".")
fails = []
try:
    import orders

    for cents in (1999, 5, 120000):
        o = {"id": "o-" + str(cents), "total_cents": cents, "currency": "USD"}
        p = orders.order_payload(o)
        json.dumps(p)
        if p.get("id") != o["id"] or p.get("currency") != "USD":
            fails.append("id or currency changed in the payload")
        t = p.get("total")
        if type(t) is not float or abs(t - cents / 100) > 1e-9:
            fails.append("total is %r for %d cents; partners parse it as dollars, so it must stay %r" % (t, cents, cents / 100))
        new = [k for k, v in p.items() if k not in ("id", "total", "currency") and type(v) is int and v == cents]
        if not new:
            fails.append("no field carries the total as integer cents")
        elif not any(k in open("PARTNERS.md").read() for k in new):
            fails.append("the new field " + new[0] + " is not documented in PARTNERS.md")
except Exception as e:
    fails.append("orders.order_payload is not usable: %r" % (e,))
fails = list(dict.fromkeys(fails))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
