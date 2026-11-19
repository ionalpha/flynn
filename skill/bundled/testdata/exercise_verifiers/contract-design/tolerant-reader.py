import subprocess
import sys

sys.path.insert(0, ".")
fails = []
KNOWN = ("paid", "refunded", "failed")
base = {"id": "e1", "status": "paid", "amount_cents": 500}
try:
    import events

    for extra in ({"region": "eu"}, {"zz_added_next_year": {"nested": [1, 2]}}, {"region": "us", "channel": "pos"}):
        try:
            e = events.parse_event(dict(base, **extra))
            if e.status != "paid" or e.amount_cents != 500:
                fails.append("an event with %s parsed to the wrong values" % sorted(extra))
        except Exception as ex:
            fails.append("an event carrying %s is rejected: %r" % (sorted(extra), ex))
    try:
        e = events.parse_event(dict(base, status="disputed"))
        if getattr(e, "status", None) in KNOWN:
            fails.append("an unknown status was read as " + e.status)
    except Exception as ex:
        fails.append("an event with a status the parser has not seen is rejected: %r" % (ex,))
    for bad in (dict(base, amount_cents="500"), {"id": "e1", "status": "paid"}, dict(base, amount_cents=None), dict(base, amount_cents=True), dict(base, amount_cents=5.5)):
        try:
            events.parse_event(bad)
            fails.append("a malformed amount_cents is accepted: %r" % (bad.get("amount_cents", "missing"),))
        except Exception:
            pass
except Exception as e:
    fails.append("events.parse_event is not usable: %r" % (e,))
p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-q"], capture_output=True, encoding="utf-8", errors="replace", timeout=60)
if p.returncode != 0:
    fails.append("the tests fail: " + p.stderr[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
