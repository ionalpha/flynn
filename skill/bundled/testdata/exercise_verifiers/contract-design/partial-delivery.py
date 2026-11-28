import json
import sys

sys.path.insert(0, ".")
fails = []
OLD = {"id", "status", "carrier", "eta"}
STATUSES = {"pending", "in_transit", "delivered", "returned"}


def ship(delivered, qty=(2, 3), dispatched=True, returned=False):
    lines = [{"sku": "a%d" % i, "qty": q, "delivered_qty": d} for i, (q, d) in enumerate(zip(qty, delivered))]
    return {"id": "s9", "carrier": "dhl", "eta": "2026-03-04", "dispatched": dispatched, "returned": returned, "lines": lines}


def signals_partial(p):
    for k, v in p.items():
        if k in OLD:
            continue
        if isinstance(v, str) and "partial" in v.lower():
            return True
        if "partial" in k.lower() and v not in (None, False, 0, "", "none", "no"):
            return True
    return False


try:
    import labels
    import shipments

    cases = [
        ("pending", ship((0, 0), dispatched=False), "pending", False),
        ("in transit, nothing arrived", ship((0, 0)), "in_transit", False),
        ("delivered", ship((2, 3)), "delivered", False),
        ("returned", ship((2, 3), returned=True), "returned", False),
        ("two of five units arrived", ship((2, 0)), "in_transit", True),
        ("one unit short", ship((2, 2)), "in_transit", True),
    ]
    for name, s, want, partial in cases:
        p = shipments.shipment_payload(s)
        json.dumps(p)
        if not OLD <= set(p):
            fails.append("%s: a published field is gone: %s" % (name, sorted(OLD - set(p))))
            continue
        if p["status"] not in STATUSES:
            fails.append("%s: status %r is outside the four values partners were given and route on" % (name, p["status"]))
        elif p["status"] != want:
            fails.append("%s: status is %r, want %r" % (name, p["status"], want))
        if (p["id"], p["carrier"], p["eta"]) != ("s9", "dhl", "2026-03-04"):
            fails.append("%s: id, carrier or eta changed" % name)
        if signals_partial(p) != partial:
            fails.append("%s: partial delivery %s" % (name, "is not visible in any new field" if partial else "is reported for a shipment that is not partial"))
        try:
            labels.label(p["status"])
        except Exception as e:
            fails.append("%s: the dashboard cannot label %r: %r" % (name, p["status"], e))
except Exception as e:
    fails.append("shipment_payload is not usable: %r" % (e,))
for f in dict.fromkeys(fails):
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
